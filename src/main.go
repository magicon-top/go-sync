package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jlaffaye/ftp"
	ignore "github.com/monochromegane/go-gitignore"
	"golang.org/x/sys/windows"
)

type SyncRule struct {
	BaseDir string // Directory where .go-sync.txt was found
	Pattern string // Clean pattern without base path
	Action  string // Action to perform
	Target  string // Target path or command
}

type CachedRules struct {
	ModTime time.Time // Modification time of the config file
	Size    int64     // Size of the config file
	Rules   []SyncRule // Parsed rules
}

type FTPConnection struct {
	Conn *ftp.ServerConn // Active FTP connection
	Mu   sync.Mutex      // Mutex for thread-safe operations
}

var (
	ftpConns       = make(map[string]*FTPConnection) // Cache for FTP connections
	ftpMutex       sync.Mutex                        // Mutex for FTP cache
	logMutex       sync.Mutex                        // Mutex for log file
	ruleCache      = make(map[string]CachedRules)    // Cache for parsed rules
	ruleCacheMutex sync.Mutex                        // Mutex for rules cache
)

func init() {
	enableVirtualTerminalProcessing() // Enable colored output on startup
}

//________________________________________________________
//Enables ANSI escape codes in Windows console
func enableVirtualTerminalProcessing() {
	stdout := windows.Handle(os.Stdout.Fd()) // Get stdout handle
	var mode uint32                          // Mode variable
	windows.GetConsoleMode(stdout, &mode)    // Get current mode
	windows.SetConsoleMode(stdout, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) // Set new mode
}

//________________________________________________________
//Prints colored event string to the console
func printColoredEvent(action uint32, isDir bool, text string) {
	var colorCode string // ANSI color code
	reset := "\033[0m"   // ANSI reset code

	switch action {
	case windows.FILE_ACTION_ADDED:
		if isDir {
			colorCode = "\033[42;37m" // Green BG, White FG
		} else {
			colorCode = "\033[92m" // Bright Green FG
		}
	case windows.FILE_ACTION_REMOVED:
		if isDir {
			colorCode = "\033[41;37m" // Red BG, White FG
		} else {
			colorCode = "\033[91m" // Bright Red FG
		}
	case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
		if isDir {
			colorCode = "\033[46;37m" // Cyan BG, White FG
		} else {
			colorCode = "\033[96m" // Bright Cyan FG
		}
	case windows.FILE_ACTION_MODIFIED:
		if isDir {
			colorCode = "\033[43;30m" // Yellow BG, Black FG
		} else {
			colorCode = "\033[93m" // Bright Yellow FG
		}
	default:
		colorCode = "\033[37m" // White FG
	}

	timestamp := time.Now().Format("15:04:05") // Current time
	fmt.Printf("[%s] %s%s%s\n", timestamp, colorCode, text, reset) // Print formatted string
}

//________________________________________________________
//Writes log entries with size check and rotation at 50MB
//________________________________________________________
//Writes log entries with size check and rotation at 50MB
func logToFile(opType, detail string) {
	logMutex.Lock()         // Lock log file
	defer logMutex.Unlock() // Unlock log file on exit

	// Get the directory where the program was started from
	exePath, err := os.Executable() // Get executable path
	if err != nil {
		return // Exit on error
	}

	exeDir := filepath.Dir(exePath) // Get executable directory
	
	// Alternative: use working directory instead of executable directory
	// workDir, err := os.Getwd()
	// if err != nil {
	//     return
	// }
	// exeDir = workDir
	
	logPath := filepath.Join(exeDir, "go-sync.log") // Construct log file path
	oldLogPath := filepath.Join(exeDir, "go-sync_old.log") // Construct old log file path

	if fi, err := os.Stat(logPath); err == nil { // Check log file size
		if fi.Size() >= 50*1024*1024 { // Rotate if >= 50MB
			os.Remove(oldLogPath) // Remove old log
			os.Rename(logPath, oldLogPath) // Rename current to old
		}
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) // Open log file
	if err != nil {
		return // Exit on error
	}
	defer file.Close() // Close on exit

	timestamp := time.Now().Format("2006-01-02 15:04:05") // Format timestamp
	logEntry := fmt.Sprintf("[%s] %s | %s\n", timestamp, opType, detail) // Format log entry
	file.WriteString(logEntry) // Write to file
}
//________________________________________________________
//Loads .watchignore rules from file near executable
func loadIgnoreRules() ignore.IgnoreMatcher {
	exePath, err := os.Executable() // Get executable path
	var ignorePath string           // Path to ignore file

	if err == nil {
		ignorePath = filepath.Join(filepath.Dir(exePath), ".watchignore") // Use absolute path
	} else {
		ignorePath = ".watchignore" // Use relative path as fallback
	}

	matcher, err := ignore.NewGitIgnore(ignorePath) // Create matcher
	if err != nil {
		return nil // Return nil if file not found
	}

	return matcher // Return initialized matcher
}

//________________________________________________________
//Parses .go-sync.txt and creates rules
func parseSyncFile(filePath, configDir string) ([]SyncRule, error) {
	file, err := os.Open(filePath) // Open config file
	if err != nil {
		return nil, err // Return on error
	}
	defer file.Close() // Close on exit

	var rules []SyncRule              // Rules slice
	scanner := bufio.NewScanner(file) // Initialize scanner

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text()) // Trim line

		if line == "" || strings.HasPrefix(line, "#") { // Skip empty or commented lines
			continue 
		}

		// Check if line starts with action (no pattern specified)
		var rawPattern, action string
		
		if strings.HasPrefix(line, ">") || strings.HasPrefix(line, "<>") {
			// Line starts with action, no pattern specified
			rawPattern = "*" // Use wildcard for all files
			action = line    // Entire line is action
		} else {
			// Standard format: pattern;action
			parts := strings.SplitN(line, ";", 2) // Split by semicolon
			if len(parts) != 2 {
				continue // Skip invalid lines
			}

			rawPattern = strings.TrimSpace(parts[0]) // Get raw pattern
			action = strings.TrimSpace(parts[1])     // Get raw action
		}

		if rawPattern == "" || action == "" { // Check for empty parts
			continue
		}

		rule := SyncRule{
			BaseDir: configDir,  // Store config directory
			Pattern: rawPattern, // Store clean pattern (or "*" for all)
		}

		if strings.HasPrefix(action, "<> ftp://") { // Parse FTP sync
			rule.Action = "ftp_sync"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "<> "))
		} else if strings.HasPrefix(action, "> ftp://") { // Parse FTP copy
			rule.Action = "ftp_copy"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "> "))
		} else if strings.HasPrefix(action, "<>") { // Parse local sync
			rule.Action = "sync"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "<>"))
		} else if strings.HasPrefix(action, ">") { // Parse local copy
			rule.Action = "copy"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, ">"))
		} else {
			// Check if this is a command (run action)
			rule.Action = "run"
			rule.Target = action
		}

		rules = append(rules, rule) // Append parsed rule
	}

	return rules, nil // Return parsed rules
}

//________________________________________________________
//Loads rules from .go-sync.txt with caching based on ModTime and Size
func loadRulesCached(configPath string, configDir string) ([]SyncRule, error) {
	fi, err := os.Stat(configPath) // Get file stats
	if err != nil {
		return nil, err // Return on error
	}

	ruleCacheMutex.Lock()         // Lock cache mutex
	defer ruleCacheMutex.Unlock() // Unlock cache mutex on exit

	cached, exists := ruleCache[configPath] // Try to get cached rules
	if exists && cached.ModTime.Equal(fi.ModTime()) && cached.Size == fi.Size() { // Check if unchanged
		return cached.Rules, nil // Return cached rules
	}

	rules, err := parseSyncFile(configPath, configDir) // Parse file again if changed
	if err != nil {
		return nil, err // Return on parse error
	}

	ruleCache[configPath] = CachedRules{
		ModTime: fi.ModTime(), // Update ModTime
		Size:    fi.Size(),    // Update Size
		Rules:   rules,        // Update Rules
	}
	return rules, nil // Return new rules
}

//________________________________________________________
//Traverses up from file's directory to drive root, collecting all .go-sync.txt rules
func getRulesForPath(filePath string) []SyncRule {
	var allRules []SyncRule // Slice for all rules

	dir := filepath.Dir(filePath) // Get file directory
	drive := filepath.VolumeName(dir) + "\\" // Get drive root
	if drive == "\\" || drive == "" {
		drive = "D:\\" // Fallback to D:
	}

	curr := dir // Current directory pointer
	for {
		configPath := filepath.Join(curr, ".go-sync.txt") // Construct config path
		if rules, err := loadRulesCached(configPath, curr); err == nil { // Load rules
			allRules = append(allRules, rules...) // Append to all rules
		}

		parent := filepath.Dir(curr) // Get parent directory
		if parent == curr || len(curr) <= len(drive) { // Stop at root
			break
		}
		curr = parent // Move up
	}

	return allRules // Return collected rules
}

//________________________________________________________
//Parse FTP URL
func parseFTPURL(url string) (host, user, pass, path string, err error) {
	if strings.HasPrefix(url, "ftp://") { // Trim prefix
		url = strings.TrimPrefix(url, "ftp://")
	}

	parts := strings.SplitN(url, "/", 2) // Split host and path
	if len(parts) == 0 {
		return "", "", "", "", fmt.Errorf("invalid FTP URL") // Return on invalid URL
	}

	hostPart := parts[0] // Extract host part
	path = "/"           // Default path
	if len(parts) == 2 {
		path = "/" + parts[1] // Extract path
	}

	user = "anonymous" // Default user
	pass = "anonymous" // Default password

	if strings.Contains(hostPart, "@") { // Parse auth credentials
		authHost := strings.SplitN(hostPart, "@", 2) // Split auth and host
		auth := authHost[0]                          // Extract auth
		host := authHost[1]                          // Extract pure host

		if strings.Contains(auth, ":") { // Parse user and pass
			authParts := strings.SplitN(auth, ":", 2)
			user = authParts[0]
			pass = authParts[1]
		} else {
			user = auth // User only
		}
		hostPart = host // Update host part
	}

	host = hostPart // Final host

	if !strings.Contains(host, ":") { // Add default port if missing
		host = host + ":21"
	}

	return host, user, pass, path, nil // Return parsed values
}

//________________________________________________________
//Get or create FTP connection
func getFTPConnection(ftpURL string) (*FTPConnection, error) {
	ftpMutex.Lock()         // Lock FTP cache
	defer ftpMutex.Unlock() // Unlock FTP cache on exit

	if conn, exists := ftpConns[ftpURL]; exists { // Check existing connection
		if err := conn.Conn.NoOp(); err == nil { // Keepalive check
			return conn, nil // Return active connection
		}
		conn.Conn.Quit() // Close dead connection
		delete(ftpConns, ftpURL) // Remove from cache
	}

	host, user, pass, _, err := parseFTPURL(ftpURL) // Parse URL
	if err != nil {
		return nil, err // Return on parse error
	}

	conn, err := ftp.Dial(host, ftp.DialWithTimeout(5*time.Second)) // Dial FTP
	if err != nil {
		return nil, fmt.Errorf("FTP dial error: %v", err) // Return on dial error
	}

	err = conn.Login(user, pass) // Login
	if err != nil {
		conn.Quit() // Close on error
		return nil, fmt.Errorf("FTP login error: %v", err) // Return on login error
	}

	ftpConn := &FTPConnection{
		Conn: conn, // Initialize struct
	}

	ftpConns[ftpURL] = ftpConn // Add to cache
	return ftpConn, nil        // Return new connection
}

//________________________________________________________
//Force closes and removes FTP connection from cache
func forceCloseFTP(ftpURL string) {
	ftpMutex.Lock()         // Lock FTP cache
	defer ftpMutex.Unlock() // Unlock FTP cache on exit
	if conn, exists := ftpConns[ftpURL]; exists { // Check if exists
		conn.Conn.Quit()         // Close connection
		delete(ftpConns, ftpURL) // Remove from cache
	}
}

//________________________________________________________
//Upload wrapper with retry mechanism
func uploadToFTP(localPath, ftpURL string, rule SyncRule) error {
	var err error // Error variable
	for attempt := 1; attempt <= 3; attempt++ { // Retry up to 3 times
		err = doUploadToFTP(localPath, ftpURL, rule) // Try upload
		if err == nil {
			return nil // Success
		}
		fmt.Printf("[ERROR] FTP upload attempt %d failed: %v\n", attempt, err) // Log error
		forceCloseFTP(ftpURL) // Close broken connection
		time.Sleep(1 * time.Second) // Wait before retry
	}
	return err // Return final error
}

//________________________________________________________
//Core upload file or directory to FTP logic
func doUploadToFTP(localPath, ftpURL string, rule SyncRule) error {
	ftpConn, err := getFTPConnection(ftpURL) // Get connection
	if err != nil {
		return err // Return on error
	}

	ftpConn.Mu.Lock()         // Lock connection mutex
	defer ftpConn.Mu.Unlock() // Unlock connection mutex on exit

	host, _, _, ftpPath, err := parseFTPURL(ftpURL) // Parse URL
	if err != nil {
		return err // Return on parse error
	}

	relPath := getTargetPath(localPath, rule) // Get relative path
	remotePath := filepath.ToSlash(filepath.Join(ftpPath, relPath)) // Construct remote path
	remotePath = strings.TrimPrefix(remotePath, "/") // Trim leading slash

	fi, err := os.Stat(localPath) // Stat local file
	if err != nil {
		return fmt.Errorf("Error stat local file: %v", err) // Return on stat error
	}

	if fi.IsDir() { // Handle directory creation
		err = ftpConn.Conn.MakeDir(remotePath) // Create remote directory
		if err != nil {
			// Ignore if exists
		}
		logToFile("FTP_MKDIR", fmt.Sprintf("%s -> ftp://%s/%s", localPath, host, remotePath)) // Log success
		return nil // Exit
	}

	dirs := strings.Split(filepath.ToSlash(filepath.Dir(remotePath)), "/") // Split path into dirs
	currentPath := "" // Current remote path pointer
	for _, dir := range dirs { // Iterate dirs
		if dir == "" {
			continue // Skip empty
		}
		currentPath += "/" + dir // Build path
		ftpConn.Conn.MakeDir(currentPath) // Ensure directory exists
	}

	file, err := os.Open(localPath) // Open local file
	if err != nil {
		return fmt.Errorf("Error opening local file: %v", err) // Return on open error
	}
	defer file.Close() // Close local file on exit

	err = ftpConn.Conn.Stor(remotePath, file) // Upload file
	if err != nil {
		return fmt.Errorf("FTP upload error: %v", err) // Return on upload error
	}

	logToFile("FTP_UPLOAD", fmt.Sprintf("%s -> ftp://%s/%s", localPath, host, remotePath)) // Log success
	return nil // Return nil on success
}

//________________________________________________________
//Delete wrapper with retry mechanism
func deleteFromFTP(localPath, ftpURL string, rule SyncRule, isDir bool) error {
	var err error // Error variable
	for attempt := 1; attempt <= 3; attempt++ { // Retry up to 3 times
		err = doDeleteFromFTP(localPath, ftpURL, rule, isDir) // Try delete
		if err == nil {
			return nil // Success
		}
		fmt.Printf("[ERROR] FTP delete attempt %d failed: %v\n", attempt, err) // Log error
		forceCloseFTP(ftpURL) // Close broken connection
		time.Sleep(1 * time.Second) // Wait before retry
	}
	return err // Return final error
}

//________________________________________________________
//Core delete file or directory from FTP logic
func doDeleteFromFTP(localPath, ftpURL string, rule SyncRule, isDir bool) error {
	ftpConn, err := getFTPConnection(ftpURL) // Get connection
	if err != nil {
		return err // Return on error
	}

	ftpConn.Mu.Lock()         // Lock connection mutex
	defer ftpConn.Mu.Unlock() // Unlock connection mutex on exit

	host, _, _, ftpPath, err := parseFTPURL(ftpURL) // Parse URL
	if err != nil {
		return err // Return on parse error
	}

	relPath := getTargetPath(localPath, rule) // Get relative path
	remotePath := filepath.ToSlash(filepath.Join(ftpPath, relPath)) // Construct remote path
	remotePath = strings.TrimPrefix(remotePath, "/") // Trim leading slash

	if isDir { // Handle directory deletion
		err = removeFTPDirectory(ftpConn.Conn, remotePath) // Recursive remove
		if err != nil {
			return fmt.Errorf("FTP delete directory error: %v", err) // Return on error
		}
		logToFile("FTP_RMDIR", fmt.Sprintf("ftp://%s/%s", host, remotePath)) // Log success
	} else { // Handle file deletion
		err = ftpConn.Conn.Delete(remotePath) // Delete file
		if err != nil {
			return fmt.Errorf("FTP delete error: %v", err) // Return on error
		}
		logToFile("FTP_DELETE", fmt.Sprintf("ftp://%s/%s", host, remotePath)) // Log success
	}

	return nil // Return nil on success
}

//________________________________________________________
//Recursively remove directory from FTP
func removeFTPDirectory(conn *ftp.ServerConn, path string) error {
	entries, err := conn.List(path) // List directory content
	if err != nil {
		return err // Return on list error
	}

	for _, entry := range entries { // Iterate content
		entryPath := filepath.ToSlash(filepath.Join(path, entry.Name)) // Construct entry path

		if entry.Type == ftp.EntryTypeFolder { // If directory
			err = removeFTPDirectory(conn, entryPath) // Recursive call
			if err != nil {
				return err // Return on error
			}
		} else { // If file
			err = conn.Delete(entryPath) // Delete file
			if err != nil {
				return err // Return on error
			}
		}
	}

	err = conn.RemoveDir(path) // Remove empty directory
	if err != nil {
		return err // Return on error
	}

	return nil // Return nil on success
}

//________________________________________________________
//Check if path matches standard gitignore-like inclusion pattern
func matchPattern(rule SyncRule, fullPath string) bool {
	baseDir := strings.ToLower(filepath.Clean(rule.BaseDir)) // Clean and lower BaseDir
	pathLower := strings.ToLower(filepath.Clean(fullPath))   // Clean and lower full path

	if !strings.HasPrefix(pathLower, baseDir) { // Must be inside BaseDir
		return false // Reject outside
	}

	relPath := strings.TrimPrefix(pathLower, baseDir) // Get relative path
	relPath = strings.TrimPrefix(relPath, "\\")       // Trim leading slash
	if relPath == "" {
		return false // Reject empty
	}

	patternLower := strings.ToLower(strings.ReplaceAll(rule.Pattern, "/", "\\")) // Clean pattern

	if patternLower == relPath { // Direct match
		return true // Match found
	}

	if patternLower == "**" || patternLower == "*" { // Universal wildcard
		return true // Match found
	}

	if !strings.Contains(patternLower, "\\") { // If pattern has no slashes
		matched, err := filepath.Match(patternLower, filepath.Base(relPath)) // Match base name
		if err == nil && matched {
			return true // Match found
		}
		if strings.HasPrefix(patternLower, "*") { // Support suffix match
			ext := strings.TrimPrefix(patternLower, "*") // Extract extension
			if strings.HasSuffix(relPath, ext) {
				return true // Match found
			}
		}
	}

	if strings.HasPrefix(patternLower, "**/") { // Handle patterns starting with **/
		subPattern := strings.TrimPrefix(patternLower, "**/") // Trim prefix
		if m, _ := filepath.Match(subPattern, filepath.Base(relPath)); m { // Match base name
			return true // Match found
		}
		if strings.HasSuffix(relPath, "\\"+subPattern) || strings.Contains(relPath, "\\"+subPattern) { // Match suffix or contain
			return true // Match found
		}
	}

	if strings.Contains(patternLower, "**") { // Handle generic double star
		parts := strings.Split(patternLower, "**") // Split pattern
		if len(parts) == 2 {
			prefix := strings.TrimSuffix(parts[0], "\\") // Trim suffix
			suffix := strings.TrimPrefix(parts[1], "\\") // Trim prefix

			hasPrefix := prefix == "" || strings.HasPrefix(relPath, prefix) // Check prefix
			hasSuffix := suffix == "" || strings.HasSuffix(relPath, suffix) // Check suffix

			if hasPrefix && hasSuffix { // Ensure both match
				return true // Match found
			}
		}
	}

	matched, err := filepath.Match(patternLower, relPath) // Standard glob match on relative path
	if err == nil && matched {
		return true // Match found
	}

	return false // No match found
}

//________________________________________________________
//Get target path for copy/sync operation relative to BaseDir
func getTargetPath(srcPath string, rule SyncRule) string {
	relPath, err := filepath.Rel(rule.BaseDir, srcPath) // Get relative path from BaseDir
	if err != nil {
		return filepath.Base(srcPath) // Fallback to base name on error
	}
	return relPath // Return relative path
}

//________________________________________________________
//Copy file using rule (preserving directory structure)
func copyFileWithRule(srcPath string, rule SyncRule) {
	relPath := getTargetPath(srcPath, rule) // Get relative target path
	destPath := filepath.Join(rule.Target, relPath) // Construct absolute destination path

	fi, err := os.Stat(srcPath) // Stat source path
	if err != nil {
		return // Exit on error
	}

	if fi.IsDir() { // If directory
		os.MkdirAll(destPath, 0755) // Recreate directory structure
		logToFile("MKDIR", destPath) // Log action
		return // Exit
	}

	os.MkdirAll(filepath.Dir(destPath), 0755) // Ensure target directory exists

	var data []byte // Byte slice for data
	for i := 0; i < 3; i++ { // Retry reading up to 3 times if file is temporarily locked by another process
		data, err = os.ReadFile(srcPath) // Try to read file
		if err == nil {
			break // Exit loop on success
		}
		time.Sleep(100 * time.Millisecond) // Wait before retry
	}

	if err != nil {
		logToFile("COPY_ERROR", fmt.Sprintf("Locked or denied: %s", srcPath)) // Log locked file
		return // Exit
	}

	err = os.WriteFile(destPath, data, 0644) // Write file to destination
	if err != nil {
		logToFile("COPY_ERROR", fmt.Sprintf("Write failed to %s: %v", destPath, err)) // Log write error
		return // Exit
	}

	logToFile("COPY", fmt.Sprintf("%s -> %s", srcPath, destPath)) // Log success
}

//________________________________________________________
//Execute sync rules dynamically found from .go-sync.txt hierarchy
func executeRules(filePath string, action uint32, isDir bool, oldPath string, oldIsDir bool) {
	rules := getRulesForPath(filePath) // Gather rules dynamically
	if len(rules) == 0 {
		return // Exit if no rules
	}

	for _, rule := range rules { // Iterate matched rules
		if !matchPattern(rule, filePath) { // Check relative path against BaseDir
			continue // Skip if not matched
		}

		switch rule.Action {
		case "copy":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				copyFileWithRule(filePath, rule) // Copy file
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				copyFileWithRule(filePath, rule) // Copy new file
				oldRelPath := getTargetPath(oldPath, rule) // Get old relative path
				oldTarget := filepath.Join(rule.Target, oldRelPath) // Construct old target
				os.Remove(oldTarget) // Remove old file
				logToFile("DELETE", oldTarget) // Log action
			}

		case "sync":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				copyFileWithRule(filePath, rule) // Copy file
			} else if action == windows.FILE_ACTION_REMOVED {
				relPath := getTargetPath(filePath, rule) // Get relative path
				targetPath := filepath.Join(rule.Target, relPath) // Construct target
				if isDir {
					os.RemoveAll(targetPath) // Remove directory
				} else {
					os.Remove(targetPath) // Remove file
				}
				logToFile("SYNC_DELETE", targetPath) // Log action
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				oldRelPath := getTargetPath(oldPath, rule) // Get old relative path
				newRelPath := getTargetPath(filePath, rule) // Get new relative path
				oldTarget := filepath.Join(rule.Target, oldRelPath) // Construct old target
				newTarget := filepath.Join(rule.Target, newRelPath) // Construct new target
				os.Rename(oldTarget, newTarget) // Rename target
				logToFile("SYNC_RENAME", fmt.Sprintf("%s -> %s", oldTarget, newTarget)) // Log action
			}

		case "ftp_copy":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				uploadToFTP(filePath, rule.Target, rule) // Upload to FTP
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				uploadToFTP(filePath, rule.Target, rule) // Upload new file
				deleteFromFTP(oldPath, rule.Target, rule, oldIsDir) // Delete old from FTP
			}

		case "ftp_sync":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				uploadToFTP(filePath, rule.Target, rule) // Upload to FTP
			} else if action == windows.FILE_ACTION_REMOVED {
				deleteFromFTP(filePath, rule.Target, rule, isDir) // Delete from FTP
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				uploadToFTP(filePath, rule.Target, rule) // Upload new file
				deleteFromFTP(oldPath, rule.Target, rule, oldIsDir) // Delete old from FTP
			}

		case "run":
			command := rule.Target // Get command string

			command = strings.ReplaceAll(command, "$file", filepath.Base(filePath)) // Replace $file
			command = strings.ReplaceAll(command, "$folder", filepath.Dir(filePath)) // Replace $folder
			command = strings.ReplaceAll(command, "$name", filepath.Base(filePath)) // Replace $name
			command = strings.ReplaceAll(command, "$ext", filepath.Ext(filePath)) // Replace $ext

			eventType := "modify" // Default event type
			switch action {
			case windows.FILE_ACTION_ADDED:
				eventType = "new" // Update event type
			case windows.FILE_ACTION_REMOVED:
				eventType = "del" // Update event type
			case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
				eventType = "rename" // Update event type
			}
			command = strings.ReplaceAll(command, "$event", eventType) // Replace $event

			typeStr := "file" // Default type string
			if isDir {
				typeStr = "dir" // Update type string
			}
			command = strings.ReplaceAll(command, "$type", typeStr) // Replace $type

			logToFile("RUN_COMMAND", command) // Log command execution

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) // Create timeout context
			defer cancel() // Cancel on exit

			cmd := exec.CommandContext(ctx, "cmd", "/C", command) // Initialize command
			cmd.Dir = filepath.Dir(filePath) // Set execution directory
			
			output, err := cmd.CombinedOutput() // Execute command
			if err != nil {
				if ctx.Err() == context.DeadlineExceeded {
					logToFile("RUN_TIMEOUT", fmt.Sprintf("Command timed out: %s", command)) // Log timeout
				} else {
					logToFile("RUN_ERROR", fmt.Sprintf("Error: %v | Command: %s", err, command)) // Log error
				}
			} else if len(output) > 0 {
				logToFile("RUN_OUTPUT", string(output)) // Log output
			}
		}
	}
}

//________________________________________________________
//Monitors a specific drive using native Windows API ReadDirectoryChangesW
func startWatcher(drivePath string) {
	fmt.Printf("[SYSTEM] Starting watcher for drive: %s\n", drivePath) // Print startup info

	matcher := loadIgnoreRules() // Load ignore rules
	knownDirs := make(map[string]bool) // Known directories map

	drivePtr, err := windows.UTF16PtrFromString(drivePath) // Convert drive path to pointer
	if err != nil {
		fmt.Printf("[ERROR] Creating UTF16 pointer for %s: %v\n", drivePath, err) // Print error
		return // Exit on error
	}

	handle, err := windows.CreateFile(
		drivePtr,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	) // Open directory handle
	if err != nil {
		fmt.Printf("[ERROR] Opening directory %s: %v\n", drivePath, err) // Print error
		return // Exit on error
	}
	defer windows.CloseHandle(handle) // Close handle on exit

	buffer := make([]byte, 1024*1024) // 1MB buffer to prevent event drops
	var oldFullPath string // Previous path variable
	var oldIsDir bool // Previous isDir flag

	recentEvents := make(map[string]time.Time) // Map for deduplication
	const dedupWindow = 100 * time.Millisecond // Time window for deduplication
	cleanupCounter := 0 // Counter for triggering map cleanup

	for {
		cleanupCounter++ // Increment cleanup counter
		if cleanupCounter > 1000 { // Clean up deduplication map every ~1000 events to prevent memory leak
			now := time.Now() // Get current time
			for k, v := range recentEvents { // Iterate over events
				if now.Sub(v) > dedupWindow { // Check event age
					delete(recentEvents, k) // Remove old event
				}
			}
			cleanupCounter = 0 // Reset counter
		}

		var bytesReturned uint32 // Bytes returned variable
		err := windows.ReadDirectoryChanges(
			handle,
			&buffer[0],
			uint32(len(buffer)),
			true,
			windows.FILE_NOTIFY_CHANGE_FILE_NAME|
				windows.FILE_NOTIFY_CHANGE_DIR_NAME|
				windows.FILE_NOTIFY_CHANGE_LAST_WRITE|
				windows.FILE_NOTIFY_CHANGE_SIZE|
				windows.FILE_NOTIFY_CHANGE_CREATION,
			&bytesReturned,
			nil,
			0,
		) // Call native API
		if err != nil {
			fmt.Printf("[ERROR] Reading directory changes on %s: %v\n", drivePath, err) // Print error
			time.Sleep(1 * time.Second) // Delay on error
			continue // Continue loop
		}

		if bytesReturned == 0 { // Check if empty
			continue // Continue loop
		}

		offset := uint32(0) // Initialize offset
		for {
			if offset >= bytesReturned { // Check boundary
				break // Break inner loop
			}

			info := (*windows.FileNotifyInformation)(unsafe.Pointer(&buffer[offset])) // Cast pointer to struct

			if info.FileNameLength == 0 || info.FileNameLength > 1024 { // Validate length
				break // Break inner loop
			}

			fileName := windows.UTF16ToString((*[1024]uint16)(unsafe.Pointer(&info.FileName))[0 : info.FileNameLength/2]) // Extract filename

			if fileName == "" { // Check if empty
				if info.NextEntryOffset == 0 { // Check next offset
					break // Break inner loop
				}
				offset += info.NextEntryOffset // Increment offset
				continue // Continue inner loop
			}

			normalizedPath := strings.ReplaceAll(fileName, "\\", "/") // Normalize slashes for matcher

			if matcher != nil && matcher.Match(normalizedPath, false) { // Apply ignore rules
				if info.NextEntryOffset == 0 { // Check next offset
					break // Break inner loop
				}
				offset += info.NextEntryOffset // Increment offset
				continue // Continue inner loop
			}

			fullPath := filepath.Join(drivePath, fileName) // Construct full path

			fi, statErr := os.Stat(fullPath) // Stat file
			isDir := statErr == nil && fi.IsDir() // Determine if directory

			switch info.Action {
			case windows.FILE_ACTION_ADDED:
				if isDir {
					knownDirs[fullPath] = true // Track new directory
				}

			case windows.FILE_ACTION_REMOVED:
				if knownDirs[fullPath] { // Check if known directory
					isDir = true // Restore isDir flag for deleted directories
				}

			case windows.FILE_ACTION_RENAMED_OLD_NAME:
				oldFullPath = fullPath // Store old path
				oldIsDir = isDir || knownDirs[fullPath] // Store old isDir flag

			case windows.FILE_ACTION_RENAMED_NEW_NAME:
				if isDir {
					knownDirs[fullPath] = true // Track renamed directory
				}
			}

			if isDir && info.Action == windows.FILE_ACTION_MODIFIED { // Ignore directory modifications
				if info.NextEntryOffset == 0 { // Check next offset
					break // Break inner loop
				}
				offset += info.NextEntryOffset // Increment offset
				continue // Continue inner loop
			}

			eventKey := fmt.Sprintf("%d|%s", info.Action, fullPath) // Construct event key
			if last, exists := recentEvents[eventKey]; exists && time.Since(last) < dedupWindow { // Deduplicate events
				if info.NextEntryOffset == 0 { // Check next offset
					break // Break inner loop
				}
				offset += info.NextEntryOffset // Increment offset
				continue // Continue inner loop
			}
			recentEvents[eventKey] = time.Now() // Register new event

			var msg string // Console message
			switch info.Action {
			case windows.FILE_ACTION_ADDED:
				if isDir {
					msg = fmt.Sprintf("new dir %s", fullPath) // Format message
				} else {
					msg = fmt.Sprintf("new file %s", fullPath) // Format message
				}

			case windows.FILE_ACTION_MODIFIED:
				msg = fmt.Sprintf("mod file %s", fullPath) // Format message

			case windows.FILE_ACTION_REMOVED:
				if isDir {
					msg = fmt.Sprintf("del dir %s", fullPath) // Format message
				} else {
					msg = fmt.Sprintf("del file %s", fullPath) // Format message
				}

			case windows.FILE_ACTION_RENAMED_OLD_NAME:
				msg = "" // Suppress output for old name

			case windows.FILE_ACTION_RENAMED_NEW_NAME:
				if oldFullPath != "" { // Check if previous name exists
					newBase := filepath.Base(fullPath) // Get new base name
					if oldIsDir {
						msg = fmt.Sprintf("ren dir %s %s", oldFullPath, newBase) // Format message
					} else {
						msg = fmt.Sprintf("ren file %s %s", oldFullPath, newBase) // Format message
					}
					
					go executeRules(fullPath, info.Action, isDir, oldFullPath, oldIsDir) // Execute rule for rename
					
					oldFullPath = "" // Reset old path
					oldIsDir = false // Reset old isDir flag
				} else {
					if isDir {
						msg = fmt.Sprintf("ren dir %s", fullPath) // Format message
					} else {
						msg = fmt.Sprintf("ren file %s", fullPath) // Format message
					}
				}
			}

			if msg != "" { // Check if message is ready
				printColoredEvent(info.Action, isDir, msg) // Output event

				if info.Action != windows.FILE_ACTION_RENAMED_NEW_NAME { // Filter execution
					go executeRules(fullPath, info.Action, isDir, "", false) // Execute rules asynchronously
				}

				if info.Action == windows.FILE_ACTION_REMOVED && isDir { // Cleanup known directory
					delete(knownDirs, fullPath) // Remove from map
				}
			}

			if info.NextEntryOffset == 0 { // Check next offset
				break // Break inner loop
			}
			offset += info.NextEntryOffset // Increment offset
		}
	}
}

//________________________________________________________
//Retrieves all local fixed and removable drives
func getLocalDrives() []string {
	var drives []string // Drives slice
	bitmask, err := windows.GetLogicalDrives() // Get logical drives bitmask
	if err != nil {
		return drives // Return on error
	}

	for i := 0; i < 26; i++ { // Iterate over alphabet
		if bitmask&(1<<i) != 0 { // Check if drive exists
			drive := string(rune('A'+i)) + ":\\" // Construct drive letter
			driveType := windows.GetDriveType(syscall.StringToUTF16Ptr(drive)) // Get drive type
			if driveType == windows.DRIVE_FIXED || driveType == windows.DRIVE_REMOVABLE { // Filter out CD-ROMs/Network
				drives = append(drives, drive) // Append to slice
			}
		}
	}
	return drives // Return slice
}

//________________________________________________________
//Executes cleanup logic on terminal exit
func onExit() {
	ftpMutex.Lock() // Lock FTP mutex
	for url, conn := range ftpConns { // Iterate over connections
		conn.Conn.Quit() // Close connection gracefully
		fmt.Printf("[SYSTEM] FTP disconnected: %s\n", url) // Print status
	}
	ftpMutex.Unlock() // Unlock FTP mutex
}

//________________________________________________________
//Main application entry point
func main() {
	var drivesToWatch []string // Target drives slice

	if len(os.Args) > 1 { // Check for arguments
		arg := os.Args[1] // Get first argument
		if arg == "*" { // Check for wildcard
			drivesToWatch = getLocalDrives() // Load all drives
		} else {
			arg = strings.TrimSuffix(arg, ":") // Clean argument
			arg = strings.TrimSuffix(arg, "\\") // Clean argument
			arg = strings.ToUpper(arg) // Convert to uppercase
			drive := arg + ":\\" // Construct drive path

			if _, err := os.Stat(drive); os.IsNotExist(err) { // Validate drive
				fmt.Printf("[ERROR] Drive %s not found.\n", drive) // Print error
				os.Exit(1) // Exit with code 1
			}
			drivesToWatch = append(drivesToWatch, drive) // Append targeted drive
		}
	} else {
		exePath, err := os.Executable() // Get current executable path
		if err != nil {
			exePath, _ = filepath.Abs(".") // Fallback to current directory
		}

		vol := filepath.VolumeName(exePath) // Get volume name
		if vol == "" {
			vol = "C:" // Fallback to C:
		}
		drive := vol + "\\" // Construct drive path
		drivesToWatch = append(drivesToWatch, drive) // Append targeted drive
	}

	if len(drivesToWatch) == 0 { // Validate targets
		fmt.Println("[ERROR] No valid drives to watch found.") // Print error
		os.Exit(1) // Exit with code 1
	}

	c := make(chan os.Signal, 1) // Signal channel
	signal.Notify(c, os.Interrupt, syscall.SIGTERM) // Register signals
	go func() { // Start signal listener routine
		<-c // Wait for signal
		fmt.Println("\n[SYSTEM] Shutting down...") // Print shutdown message
		onExit() // Run cleanup routines
		os.Exit(0) // Exit gracefully
	}()

	var wg sync.WaitGroup // Wait group
	for _, drive := range drivesToWatch { // Iterate target drives
		wg.Add(1) // Add to wait group
		go func(d string) { // Start watcher routine
			defer wg.Done() // Signal completion
			startWatcher(d) // Launch watcher
		}(drive)
	}

	fmt.Println("[SYSTEM] Application is running. Press Ctrl+C to exit.") // Print ready state
	wg.Wait() // Block main thread indefinitely
}