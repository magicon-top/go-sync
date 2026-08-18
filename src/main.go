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
	Pattern string
	Action  string
	Target  string
}

type CachedRules struct {
	ModTime time.Time
	Size    int64
	Rules   []SyncRule
}

type FTPConnection struct {
	Conn *ftp.ServerConn
	Mu   sync.Mutex
}

var (
	ftpConns       = make(map[string]*FTPConnection)
	ftpMutex       sync.Mutex
	logMutex       sync.Mutex

	ruleCache      = make(map[string]CachedRules)
	ruleCacheMutex sync.Mutex
)

func init() {
	enableVirtualTerminalProcessing()
}

//________________________________________________________
//Enables ANSI escape codes in Windows console
func enableVirtualTerminalProcessing() {
	stdout := windows.Handle(os.Stdout.Fd()) // Get stdout handle
	var mode uint32
	windows.GetConsoleMode(stdout, &mode) // Get console mode
	windows.SetConsoleMode(stdout, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) // Enable virtual terminal processing
}

//________________________________________________________
//Prints colored event string to the console
func printColoredEvent(action uint32, isDir bool, text string) {
	var colorCode string
	reset := "\033[0m" // Reset color code

	switch action {
	case windows.FILE_ACTION_ADDED:
		if isDir {
			colorCode = "\033[42;37m" // Green BG, White FG
		} else {
			colorCode = "\033[92m"    // Bright Green FG
		}
	case windows.FILE_ACTION_REMOVED:
		if isDir {
			colorCode = "\033[41;37m" // Red BG, White FG
		} else {
			colorCode = "\033[91m"    // Bright Red FG
		}
	case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
		if isDir {
			colorCode = "\033[46;37m" // Cyan BG, White FG
		} else {
			colorCode = "\033[96m"    // Bright Cyan FG
		}
	case windows.FILE_ACTION_MODIFIED:
		if isDir {
			colorCode = "\033[43;30m" // Yellow BG, Black FG
		} else {
			colorCode = "\033[93m"    // Bright Yellow FG
		}
	default:
		colorCode = "\033[37m"        // White FG
	}

	timestamp := time.Now().Format("15:04:05") // Format current time
	fmt.Printf("[%s] %s%s%s\n", timestamp, colorCode, text, reset) // Print colored log
}

//________________________________________________________
//Writes log entries with size check and rotation at 50MB
func logToFile(opType, detail string) {
	logMutex.Lock() // Lock log mutex
	defer logMutex.Unlock() // Unlock log mutex

	exePath, err := os.Executable() // Get executable path
	if err != nil {
		return
	}

	exeDir := filepath.Dir(exePath) // Get executable directory
	logPath := filepath.Join(exeDir, "go-sync.log") // Log path
	oldLogPath := filepath.Join(exeDir, "go-sync_old.log") // Old log path

	if fi, err := os.Stat(logPath); err == nil { // Check log file info
		if fi.Size() >= 50*1024*1024 { // Check if size >= 50MB
			os.Remove(oldLogPath) // Remove old log file
			os.Rename(logPath, oldLogPath) // Rotate log file
		}
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) // Open log file
	if err != nil {
		return
	}
	defer file.Close() // Close file

	timestamp := time.Now().Format("2006-01-02 15:04:05") // Format timestamp
	logEntry := fmt.Sprintf("[%s] %s | %s\n", timestamp, opType, detail) // Format log entry
	file.WriteString(logEntry) // Write log entry
}

//________________________________________________________
//Loads .watchignore rules from file near executable
func loadIgnoreRules() ignore.IgnoreMatcher {
	exePath, err := os.Executable() // Get executable path
	var ignorePath string

	if err == nil {
		ignorePath = filepath.Join(filepath.Dir(exePath), ".watchignore") // Ignore path near exe
	} else {
		ignorePath = ".watchignore" // Fallback ignore path
	}

	matcher, err := ignore.NewGitIgnore(ignorePath) // Create gitignore matcher
	if err != nil {
		return nil
	}

	return matcher
}

//________________________________________________________
//Parses .go-sync.txt and converts relative patterns to absolute based on config directory
func parseSyncFile(filePath, configDir string) ([]SyncRule, error) {
	file, err := os.Open(filePath) // Open sync file
	if err != nil {
		return nil, err
	}
	defer file.Close() // Close file

	var rules []SyncRule
	scanner := bufio.NewScanner(file) // Create scanner

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text()) // Read line

		if line == "" || strings.HasPrefix(line, "#") { // Skip empty lines and comments
			continue
		}

		parts := strings.SplitN(line, ";", 2) // Split pattern and action
		if len(parts) != 2 {
			continue
		}

		rawPattern := strings.TrimSpace(parts[0]) // Get raw pattern
		action := strings.TrimSpace(parts[1]) // Get action string

		if rawPattern == "" || action == "" {
			continue
		}

		var absPattern string
		if filepath.IsAbs(rawPattern) {
			absPattern = rawPattern // Use absolute pattern as is
		} else {
			absPattern = filepath.Join(configDir, rawPattern) // Make pattern absolute relative to config directory
		}

		rule := SyncRule{
			Pattern: absPattern,
		}

		if strings.HasPrefix(action, "<> ftp://") {
			rule.Action = "ftp_sync"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "<> "))
		} else if strings.HasPrefix(action, "> ftp://") {
			rule.Action = "ftp_copy"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "> "))
		} else if strings.HasPrefix(action, "<>") {
			rule.Action = "sync"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "<>"))
		} else if strings.HasPrefix(action, ">") {
			rule.Action = "copy"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, ">"))
		} else {
			rule.Action = "run"
			rule.Target = action
		}

		rules = append(rules, rule)
	}

	return rules, nil
}

//________________________________________________________
//Loads rules from .go-sync.txt with caching based on ModTime and Size
func loadRulesCached(configPath string, configDir string) ([]SyncRule, error) {
	fi, err := os.Stat(configPath) // Get file info for cache check
	if err != nil {
		return nil, err
	}

	ruleCacheMutex.Lock() // Lock cache mutex
	defer ruleCacheMutex.Unlock() // Unlock cache mutex

	cached, exists := ruleCache[configPath] // Check cache existence
	if exists && cached.ModTime.Equal(fi.ModTime()) && cached.Size == fi.Size() {
		return cached.Rules, nil // Return cached rules if file hasn't changed
	}

	rules, err := parseSyncFile(configPath, configDir) // Parse sync file
	if err != nil {
		return nil, err
	}

	ruleCache[configPath] = CachedRules{
		ModTime: fi.ModTime(),
		Size:    fi.Size(),
		Rules:   rules,
	} // Save rules to cache
	return rules, nil
}

//________________________________________________________
//Traverses up from file's directory to drive root, collecting all .go-sync.txt rules
func getRulesForPath(filePath string) []SyncRule {
	var allRules []SyncRule

	dir := filepath.Dir(filePath) // Get directory of event file
	drive := filepath.VolumeName(dir) + "\\" // Get drive root
	if drive == "\\" || drive == "" {
		drive = "D:\\" // Fallback drive root
	}

	curr := dir
	for {
		configPath := filepath.Join(curr, ".go-sync.txt") // Construct config path
		if rules, err := loadRulesCached(configPath, curr); err == nil {
			allRules = append(allRules, rules...) // Append loaded rules
		}

		parent := filepath.Dir(curr) // Get parent directory
		if parent == curr || len(curr) <= len(drive) {
			break
		}
		curr = parent
	}

	return allRules
}

//________________________________________________________
//Parse FTP URL
func parseFTPURL(url string) (host, user, pass, path string, err error) {
	if strings.HasPrefix(url, "ftp://") {
		url = strings.TrimPrefix(url, "ftp://") // Trim protocol prefix
	}

	parts := strings.SplitN(url, "/", 2) // Split host and path
	if len(parts) == 0 {
		return "", "", "", "", fmt.Errorf("invalid FTP URL")
	}

	hostPart := parts[0]
	path = "/"
	if len(parts) == 2 {
		path = "/" + parts[1] // Set remote path
	}

	user = "anonymous"
	pass = "anonymous"

	if strings.Contains(hostPart, "@") {
		authHost := strings.SplitN(hostPart, "@", 2)
		auth := authHost[0]
		host := authHost[1]

		if strings.Contains(auth, ":") {
			authParts := strings.SplitN(auth, ":", 2)
			user = authParts[0]
			pass = authParts[1]
		} else {
			user = auth
		}
		hostPart = host
	}

	host = hostPart

	if !strings.Contains(host, ":") {
		host = host + ":21" // Default FTP port
	}

	return host, user, pass, path, nil
}

//________________________________________________________
//Get or create FTP connection
func getFTPConnection(ftpURL string) (*FTPConnection, error) {
	ftpMutex.Lock() // Lock FTP mutex
	defer ftpMutex.Unlock() // Unlock FTP mutex

	if conn, exists := ftpConns[ftpURL]; exists {
		if err := conn.Conn.NoOp(); err == nil {
			return conn, nil // Return existing valid connection
		}
		conn.Conn.Quit() // Quit broken connection
		delete(ftpConns, ftpURL) // Delete from map
	}

	host, user, pass, _, err := parseFTPURL(ftpURL) // Parse FTP URL credentials
	if err != nil {
		return nil, err
	}

	conn, err := ftp.Dial(host, ftp.DialWithTimeout(5*time.Second)) // Dial FTP server
	if err != nil {
		return nil, fmt.Errorf("FTP dial error: %v", err)
	}

	err = conn.Login(user, pass) // Login to FTP server
	if err != nil {
		conn.Quit()
		return nil, fmt.Errorf("FTP login error: %v", err)
	}

	ftpConn := &FTPConnection{
		Conn: conn,
	}

	ftpConns[ftpURL] = ftpConn // Cache connection
	return ftpConn, nil
}

//________________________________________________________
//Force closes and removes FTP connection from cache
func forceCloseFTP(ftpURL string) {
	ftpMutex.Lock() // Lock FTP mutex
	defer ftpMutex.Unlock() // Unlock FTP mutex
	if conn, exists := ftpConns[ftpURL]; exists {
		conn.Conn.Quit() // Quit connection
		delete(ftpConns, ftpURL) // Delete from cache
	}
}

//________________________________________________________
//Upload wrapper with retry mechanism
func uploadToFTP(localPath, ftpURL string, rule SyncRule) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = doUploadToFTP(localPath, ftpURL, rule) // Try upload
		if err == nil {
			return nil
		}
		fmt.Printf("[ERROR] FTP upload attempt %d failed: %v\n", attempt, err)
		forceCloseFTP(ftpURL) // Force close connection on failure
		time.Sleep(1 * time.Second) // Wait before retry
	}
	return err
}

//________________________________________________________
//Core upload file or directory to FTP logic
func doUploadToFTP(localPath, ftpURL string, rule SyncRule) error {
	ftpConn, err := getFTPConnection(ftpURL) // Get FTP connection
	if err != nil {
		return err
	}

	ftpConn.Mu.Lock() // Lock connection mutex
	defer ftpConn.Mu.Unlock() // Unlock connection mutex

	host, _, _, ftpPath, err := parseFTPURL(ftpURL) // Parse FTP URL
	if err != nil {
		return err
	}

	relPath := getTargetPath(localPath, rule) // Get relative target path
	remotePath := filepath.ToSlash(filepath.Join(ftpPath, relPath)) // Convert to remote slash path
	remotePath = strings.TrimPrefix(remotePath, "/")

	fi, err := os.Stat(localPath) // Stat local path
	if err != nil {
		return fmt.Errorf("Error stat local file: %v", err)
	}

	if fi.IsDir() {
		err = ftpConn.Conn.MakeDir(remotePath) // Make remote directory
		if err != nil {
			// Ignore if exists
		}
		logToFile("FTP_MKDIR", fmt.Sprintf("%s -> ftp://%s/%s", localPath, host, remotePath)) // Log FTP directory creation
		return nil
	}

	dirs := strings.Split(filepath.ToSlash(filepath.Dir(remotePath)), "/")
	currentPath := ""
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		currentPath += "/" + dir
		ftpConn.Conn.MakeDir(currentPath) // Ensure remote directory exists
	}

	file, err := os.Open(localPath) // Open local file
	if err != nil {
		return fmt.Errorf("Error opening local file: %v", err)
	}
	defer file.Close() // Close file

	err = ftpConn.Conn.Stor(remotePath, file) // Upload file storage
	if err != nil {
		return fmt.Errorf("FTP upload error: %v", err)
	}

	logToFile("FTP_UPLOAD", fmt.Sprintf("%s -> ftp://%s/%s", localPath, host, remotePath)) // Log upload
	return nil
}

//________________________________________________________
//Delete wrapper with retry mechanism
func deleteFromFTP(localPath, ftpURL string, rule SyncRule, isDir bool) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = doDeleteFromFTP(localPath, ftpURL, rule, isDir) // Try delete
		if err == nil {
			return nil
		}
		fmt.Printf("[ERROR] FTP delete attempt %d failed: %v\n", attempt, err)
		forceCloseFTP(ftpURL) // Force close connection on failure
		time.Sleep(1 * time.Second) // Wait before retry
	}
	return err
}

//________________________________________________________
//Core delete file or directory from FTP logic
func doDeleteFromFTP(localPath, ftpURL string, rule SyncRule, isDir bool) error {
	ftpConn, err := getFTPConnection(ftpURL) // Get FTP connection
	if err != nil {
		return err
	}

	ftpConn.Mu.Lock() // Lock connection mutex
	defer ftpConn.Mu.Unlock() // Unlock connection mutex

	host, _, _, ftpPath, err := parseFTPURL(ftpURL) // Parse FTP URL
	if err != nil {
		return err
	}

	relPath := getTargetPath(localPath, rule) // Get relative path
	remotePath := filepath.ToSlash(filepath.Join(ftpPath, relPath)) // Convert to slash path
	remotePath = strings.TrimPrefix(remotePath, "/")

	if isDir {
		err = removeFTPDirectory(ftpConn.Conn, remotePath) // Remove remote directory recursively
		if err != nil {
			return fmt.Errorf("FTP delete directory error: %v", err)
		}
		logToFile("FTP_RMDIR", fmt.Sprintf("ftp://%s/%s", host, remotePath)) // Log remote directory removal
	} else {
		err = ftpConn.Conn.Delete(remotePath) // Delete remote file
		if err != nil {
			return fmt.Errorf("FTP delete error: %v", err)
		}
		logToFile("FTP_DELETE", fmt.Sprintf("ftp://%s/%s", host, remotePath)) // Log remote file deletion
	}

	return nil
}

//________________________________________________________
//Recursively remove directory from FTP
func removeFTPDirectory(conn *ftp.ServerConn, path string) error {
	entries, err := conn.List(path) // List directory entries
	if err != nil {
		return err
	}

	for _, entry := range entries {
		entryPath := filepath.ToSlash(filepath.Join(path, entry.Name)) // Construct entry path

		if entry.Type == ftp.EntryTypeFolder {
			err = removeFTPDirectory(conn, entryPath) // Recursive call for subfolder
			if err != nil {
				return err
			}
		} else {
			err = conn.Delete(entryPath) // Delete file entry
			if err != nil {
				return err
			}
		}
	}

	err = conn.RemoveDir(path) // Remove empty directory
	if err != nil {
		return err
	}

	return nil
}

//________________________________________________________
//Check if path matches pattern
func matchPattern(pattern, path string) bool {
	pattern = strings.ReplaceAll(pattern, "/", "\\") // Normalize separators
	path = strings.ReplaceAll(path, "/", "\\")

	patternLower := strings.ToLower(pattern) // Convert to lowercase
	pathLower := strings.ToLower(path)

	if strings.Contains(patternLower, "**") {
		parts := strings.Split(patternLower, "**") // Handle double star pattern
		if len(parts) == 2 {
			return strings.HasPrefix(pathLower, parts[0]) && strings.HasSuffix(pathLower, parts[1])
		}
	}

	if strings.ContainsAny(patternLower, "*?") {
		matched, err := filepath.Match(patternLower, pathLower) // Match file pattern
		if err == nil && matched {
			return true
		}

		_, fileName := filepath.Split(path)
		matched, err = filepath.Match(patternLower, strings.ToLower(fileName)) // Match filename only
		if err == nil && matched {
			return true
		}
	}

	if patternLower == pathLower {
		return true
	}

	return false
}

//________________________________________________________
//Get target path for copy/sync operation
func getTargetPath(srcPath string, rule SyncRule) string {
	basePattern := rule.Pattern

	if idx := strings.Index(basePattern, "**"); idx != -1 {
		basePattern = basePattern[:idx] // Strip wildcard suffix
	} else if idx := strings.Index(basePattern, "*"); idx != -1 {
		basePattern = basePattern[:idx]
	}

	basePattern = strings.ReplaceAll(basePattern, "/", "\\") // Normalize separators
	srcPathNorm := strings.ReplaceAll(srcPath, "/", "\\")

	relPath := ""
	if strings.HasPrefix(strings.ToLower(srcPathNorm), strings.ToLower(basePattern)) {
		relPath = srcPathNorm[len(basePattern):] // Get relative path portion
		relPath = strings.TrimPrefix(relPath, "\\")
	} else {
		relPath = filepath.Base(srcPath) // Fallback to base name
	}

	return relPath
}

//________________________________________________________
//Copy file using rule (preserving directory structure)
func copyFileWithRule(srcPath string, rule SyncRule) {
	relPath := getTargetPath(srcPath, rule) // Get relative path
	destPath := filepath.Join(rule.Target, relPath) // Construct destination path

	fi, err := os.Stat(srcPath) // Stat source path
	if err != nil {
		return
	}

	if fi.IsDir() {
		os.MkdirAll(destPath, 0755) // Create destination directory
		logToFile("MKDIR", destPath) // Log directory creation
		return
	}

	os.MkdirAll(filepath.Dir(destPath), 0755) // Ensure destination directory exists

	data, err := os.ReadFile(srcPath) // Read source file
	if err != nil {
		return
	}

	err = os.WriteFile(destPath, data, 0644) // Write destination file
	if err != nil {
		return
	}

	logToFile("COPY", fmt.Sprintf("%s -> %s", srcPath, destPath)) // Log file copy
}

//________________________________________________________
//Execute sync rules dynamically found from .go-sync.txt hierarchy
func executeRules(filePath string, action uint32, isDir bool, oldPath string, oldIsDir bool) {
	rules := getRulesForPath(filePath) // Get rules from hierarchical .go-sync.txt files
	if len(rules) == 0 {
		return
	}

	for _, rule := range rules {
		if !matchPattern(rule.Pattern, filePath) { // Check pattern match
			continue
		}

		switch rule.Action {
		case "copy":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				copyFileWithRule(filePath, rule)
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				copyFileWithRule(filePath, rule)
				oldRelPath := getTargetPath(oldPath, rule)
				oldTarget := filepath.Join(rule.Target, oldRelPath)
				os.Remove(oldTarget) // Remove old file after rename
				logToFile("DELETE", oldTarget)
			}

		case "sync":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				copyFileWithRule(filePath, rule)
			} else if action == windows.FILE_ACTION_REMOVED {
				relPath := getTargetPath(filePath, rule)
				targetPath := filepath.Join(rule.Target, relPath)
				if isDir {
					os.RemoveAll(targetPath) // Remove directory recursively
				} else {
					os.Remove(targetPath) // Remove file
				}
				logToFile("SYNC_DELETE", targetPath)
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				oldRelPath := getTargetPath(oldPath, rule)
				newRelPath := getTargetPath(filePath, rule)
				oldTarget := filepath.Join(rule.Target, oldRelPath)
				newTarget := filepath.Join(rule.Target, newRelPath)
				os.Rename(oldTarget, newTarget) // Rename target file/dir
				logToFile("SYNC_RENAME", fmt.Sprintf("%s -> %s", oldTarget, newTarget))
			}

		case "ftp_copy":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				uploadToFTP(filePath, rule.Target, rule)
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				uploadToFTP(filePath, rule.Target, rule)
				deleteFromFTP(oldPath, rule.Target, rule, oldIsDir)
			}

		case "ftp_sync":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				uploadToFTP(filePath, rule.Target, rule)
			} else if action == windows.FILE_ACTION_REMOVED {
				deleteFromFTP(filePath, rule.Target, rule, isDir)
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				uploadToFTP(filePath, rule.Target, rule)
				deleteFromFTP(oldPath, rule.Target, rule, oldIsDir)
			}

		case "run":
			command := rule.Target

			command = strings.ReplaceAll(command, "$file", filepath.Base(filePath)) // Replace $file
			command = strings.ReplaceAll(command, "$folder", filepath.Dir(filePath)) // Replace $folder
			command = strings.ReplaceAll(command, "$name", filepath.Base(filePath)) // Replace $name
			command = strings.ReplaceAll(command, "$ext", filepath.Ext(filePath)) // Replace $ext

			eventType := "modify"
			switch action {
			case windows.FILE_ACTION_ADDED:
				eventType = "new"
			case windows.FILE_ACTION_REMOVED:
				eventType = "del"
			case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
				eventType = "rename"
			}
			command = strings.ReplaceAll(command, "$event", eventType) // Replace $event

			typeStr := "file"
			if isDir {
				typeStr = "dir"
			}
			command = strings.ReplaceAll(command, "$type", typeStr) // Replace $type

			logToFile("RUN_COMMAND", command) // Log command execution

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			cmd := exec.CommandContext(ctx, "cmd", "/C", command) // Create command context
			cmd.Dir = filepath.Dir(filePath) // Set working directory

			output, err := cmd.CombinedOutput() // Execute command
			if err != nil {
				if ctx.Err() == context.DeadlineExceeded {
					logToFile("RUN_TIMEOUT", fmt.Sprintf("Command timed out: %s", command))
				} else {
					logToFile("RUN_ERROR", fmt.Sprintf("Error: %v | Command: %s", err, command))
				}
			} else if len(output) > 0 {
				logToFile("RUN_OUTPUT", string(output))
			}
		}
	}
}

//________________________________________________________
//Monitors a specific drive using native Windows API ReadDirectoryChangesW
func startWatcher(drivePath string) {
	fmt.Printf("[SYSTEM] Starting watcher for drive: %s\n", drivePath)

	matcher := loadIgnoreRules() // Load ignore rules
	knownDirs := make(map[string]bool)

	drivePtr, err := windows.UTF16PtrFromString(drivePath) // Convert drive path to UTF16 pointer
	if err != nil {
		fmt.Printf("[ERROR] Creating UTF16 pointer for %s: %v\n", drivePath, err)
		return
	}

	handle, err := windows.CreateFile(
		drivePtr,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	) // Open directory handle for monitoring
	if err != nil {
		fmt.Printf("[ERROR] Opening directory %s: %v\n", drivePath, err)
		return
	}
	defer windows.CloseHandle(handle) // Close handle on exit

	buffer := make([]byte, 1024*1024) // Allocate 1MB notification buffer
	var oldFullPath string
	var oldIsDir bool

	recentEvents := make(map[string]time.Time)
	const dedupWindow = 100 * time.Millisecond

	for {
		var bytesReturned uint32
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
		) // Read directory changes synchronously
		if err != nil {
			fmt.Printf("[ERROR] Reading directory changes on %s: %v\n", drivePath, err)
			time.Sleep(1 * time.Second)
			continue
		}

		if bytesReturned == 0 {
			continue
		}

		offset := uint32(0)
		for {
			if offset >= bytesReturned {
				break
			}

			info := (*windows.FileNotifyInformation)(unsafe.Pointer(&buffer[offset])) // Parse notification entry

			if info.FileNameLength == 0 || info.FileNameLength > 1024 {
				break
			}

			fileName := windows.UTF16ToString((*[1024]uint16)(unsafe.Pointer(&info.FileName))[0 : info.FileNameLength/2]) // Extract filename

			if fileName == "" {
				if info.NextEntryOffset == 0 {
					break
				}
				offset += info.NextEntryOffset
				continue
			}

			normalizedPath := strings.ReplaceAll(fileName, "\\", "/")

			if matcher != nil && matcher.Match(normalizedPath, false) {
				if info.NextEntryOffset == 0 {
					break
				}
				offset += info.NextEntryOffset
				continue
			}

			fullPath := filepath.Join(drivePath, fileName) // Construct full file path

			fi, statErr := os.Stat(fullPath) // Stat file path
			isDir := statErr == nil && fi.IsDir()

			switch info.Action {
			case windows.FILE_ACTION_ADDED:
				if isDir {
					knownDirs[fullPath] = true
				}

			case windows.FILE_ACTION_REMOVED:
				if knownDirs[fullPath] {
					isDir = true
				}

			case windows.FILE_ACTION_RENAMED_OLD_NAME:
				oldFullPath = fullPath
				oldIsDir = isDir || knownDirs[fullPath]

			case windows.FILE_ACTION_RENAMED_NEW_NAME:
				if isDir {
					knownDirs[fullPath] = true
				}
			}

			if isDir && info.Action == windows.FILE_ACTION_MODIFIED {
				if info.NextEntryOffset == 0 {
					break
				}
				offset += info.NextEntryOffset
				continue
			}

			eventKey := fmt.Sprintf("%d|%s", info.Action, fullPath)
			if last, exists := recentEvents[eventKey]; exists && time.Since(last) < dedupWindow {
				if info.NextEntryOffset == 0 {
					break
				}
				offset += info.NextEntryOffset
				continue
			}
			recentEvents[eventKey] = time.Now()

			var msg string
			switch info.Action {
			case windows.FILE_ACTION_ADDED:
				if isDir {
					msg = fmt.Sprintf("new dir %s", fullPath)
				} else {
					msg = fmt.Sprintf("new file %s", fullPath)
				}

			case windows.FILE_ACTION_MODIFIED:
				msg = fmt.Sprintf("mod file %s", fullPath)

			case windows.FILE_ACTION_REMOVED:
				if isDir {
					msg = fmt.Sprintf("del dir %s", fullPath)
				} else {
					msg = fmt.Sprintf("del file %s", fullPath)
				}

			case windows.FILE_ACTION_RENAMED_OLD_NAME:
				msg = ""

			case windows.FILE_ACTION_RENAMED_NEW_NAME:
				if oldFullPath != "" {
					newBase := filepath.Base(fullPath)
					if oldIsDir {
						msg = fmt.Sprintf("ren dir %s %s", oldFullPath, newBase)
					} else {
						msg = fmt.Sprintf("ren file %s %s", oldFullPath, newBase)
					}
					
					go executeRules(fullPath, info.Action, isDir, oldFullPath, oldIsDir) // Run rules asynchronously
					
					oldFullPath = ""
					oldIsDir = false
				} else {
					if isDir {
						msg = fmt.Sprintf("ren dir %s", fullPath)
					} else {
						msg = fmt.Sprintf("ren file %s", fullPath)
					}
				}
			}

			if msg != "" {
				printColoredEvent(info.Action, isDir, msg) // Print console log

				if info.Action != windows.FILE_ACTION_RENAMED_NEW_NAME {
					go executeRules(fullPath, info.Action, isDir, "", false) // Run rules asynchronously
				}

				if info.Action == windows.FILE_ACTION_REMOVED && isDir {
					delete(knownDirs, fullPath)
				}
			}

			if info.NextEntryOffset == 0 {
				break
			}
			offset += info.NextEntryOffset
		}
	}
}

//________________________________________________________
//Retrieves all local fixed and removable drives
func getLocalDrives() []string {
	var drives []string
	bitmask, err := windows.GetLogicalDrives() // Get logical drive bitmask
	if err != nil {
		return drives
	}

	for i := 0; i < 26; i++ {
		if bitmask&(1<<i) != 0 {
			drive := string(rune('A'+i)) + ":\\"
			driveType := windows.GetDriveType(syscall.StringToUTF16Ptr(drive)) // Get drive type
			if driveType == windows.DRIVE_FIXED || driveType == windows.DRIVE_REMOVABLE {
				drives = append(drives, drive)
			}
		}
	}
	return drives
}

//________________________________________________________
//Executes cleanup logic on terminal exit
func onExit() {
	ftpMutex.Lock() // Lock FTP mutex
	for url, conn := range ftpConns {
		conn.Conn.Quit() // Quit FTP connection
		fmt.Printf("[SYSTEM] FTP disconnected: %s\n", url)
	}
	ftpMutex.Unlock()
}

//________________________________________________________
//Main application entry point
func main() {
	var drivesToWatch []string

	if len(os.Args) > 1 {
		arg := os.Args[1]
		if arg == "*" {
			drivesToWatch = getLocalDrives() // Watch all local drives
		} else {
			arg = strings.TrimSuffix(arg, ":")
			arg = strings.TrimSuffix(arg, "\\")
			arg = strings.ToUpper(arg)
			drive := arg + ":\\"

			if _, err := os.Stat(drive); os.IsNotExist(err) {
				fmt.Printf("[ERROR] Drive %s not found.\n", drive)
				os.Exit(1)
			}
			drivesToWatch = append(drivesToWatch, drive) // Watch specified drive
		}
	} else {
		exePath, err := os.Executable()
		if err != nil {
			exePath, _ = filepath.Abs(".")
		}

		vol := filepath.VolumeName(exePath)
		if vol == "" {
			vol = "C:"
		}
		drive := vol + "\\"
		drivesToWatch = append(drivesToWatch, drive) // Watch current drive by default
	}

	if len(drivesToWatch) == 0 {
		fmt.Println("[ERROR] No valid drives to watch found.")
		os.Exit(1)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM) // Trap termination signals
	go func() {
		<-c
		fmt.Println("\n[SYSTEM] Shutting down...")
		onExit()
		os.Exit(0)
	}()

	var wg sync.WaitGroup
	for _, drive := range drivesToWatch {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			startWatcher(d) // Start watcher for each drive
		}(drive)
	}

	fmt.Println("[SYSTEM] Application is running. Press Ctrl+C to exit.")
	wg.Wait() // Wait for all watchers
}