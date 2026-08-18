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

type EventLog struct {
	Text       string
	ActionType uint32
	IsDir      bool
}

type SyncRule struct {
	Pattern string
	Action  string
	Target  string
}

type FTPConnection struct {
	Conn *ftp.ServerConn
	Mu   sync.Mutex
}

var (
	syncRules      []SyncRule
	syncRulesMutex sync.RWMutex
	ftpConns       = make(map[string]*FTPConnection)
	ftpMutex       sync.Mutex
	logMutex       sync.Mutex
)

func init() {
	enableVirtualTerminalProcessing()
}

//________________________________________________________
//Enables ANSI escape codes in Windows console
func enableVirtualTerminalProcessing() {
	stdout := windows.Handle(os.Stdout.Fd())
	var mode uint32
	windows.GetConsoleMode(stdout, &mode)
	windows.SetConsoleMode(stdout, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}

//________________________________________________________
//Prints colored event string to the console
func printColoredEvent(action uint32, isDir bool, text string) {
	var colorCode string
	reset := "\033[0m"

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

	timestamp := time.Now().Format("15:04:05")
	fmt.Printf("[%s] %s%s%s\n", timestamp, colorCode, text, reset)
}

//________________________________________________________
//Writes log entries with size check and rotation at 50MB
func logToFile(opType, detail string) {
	logMutex.Lock()
	defer logMutex.Unlock()

	exePath, err := os.Executable()
	if err != nil {
		return
	}

	exeDir := filepath.Dir(exePath)
	logPath := filepath.Join(exeDir, "go-sync.log")
	oldLogPath := filepath.Join(exeDir, "go-sync_old.log")

	if fi, err := os.Stat(logPath); err == nil {
		if fi.Size() >= 50*1024*1024 {
			os.Remove(oldLogPath)
			os.Rename(logPath, oldLogPath)
		}
	}

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer file.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logEntry := fmt.Sprintf("[%s] %s | %s\n", timestamp, opType, detail)
	file.WriteString(logEntry)
}

//________________________________________________________
//Loads .watchignore rules from file near executable
func loadIgnoreRules() ignore.IgnoreMatcher {
	exePath, err := os.Executable()
	var ignorePath string

	if err == nil {
		ignorePath = filepath.Join(filepath.Dir(exePath), ".watchignore")
	} else {
		ignorePath = ".watchignore"
	}

	matcher, err := ignore.NewGitIgnore(ignorePath)
	if err != nil {
		return nil
	}

	return matcher
}

//________________________________________________________
//Load sync rules from go-sync.txt
func loadSyncRules() []SyncRule {
	var rules []SyncRule

	exePath, err := os.Executable()
	if err != nil {
		return rules
	}

	rulesPath := filepath.Join(filepath.Dir(exePath), "go-sync.txt")
	file, err := os.Open(rulesPath)
	if err != nil {
		fmt.Printf("No go-sync.txt found: %v\n", err)
		return rules
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ";", 2)
		if len(parts) != 2 {
			fmt.Printf("Invalid rule at line %d: %s\n", lineNum, line)
			continue
		}

		pattern := strings.TrimSpace(parts[0])
		action := strings.TrimSpace(parts[1])

		if pattern == "" || action == "" {
			fmt.Printf("Empty pattern or action at line %d\n", lineNum)
			continue
		}

		rule := SyncRule{
			Pattern: pattern,
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

	return rules
}

//________________________________________________________
//Parse FTP URL
func parseFTPURL(url string) (host, user, pass, path string, err error) {
	if strings.HasPrefix(url, "ftp://") {
		url = strings.TrimPrefix(url, "ftp://")
	}

	parts := strings.SplitN(url, "/", 2)
	if len(parts) == 0 {
		return "", "", "", "", fmt.Errorf("invalid FTP URL")
	}

	hostPart := parts[0]
	path = "/"
	if len(parts) == 2 {
		path = "/" + parts[1]
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
	} else {
		hostPart = hostPart
	}

	host = hostPart

	if !strings.Contains(host, ":") {
		host = host + ":21"
	}

	return host, user, pass, path, nil
}

//________________________________________________________
//Get or create FTP connection
func getFTPConnection(ftpURL string) (*FTPConnection, error) {
	ftpMutex.Lock()
	defer ftpMutex.Unlock()

	if conn, exists := ftpConns[ftpURL]; exists {
		if err := conn.Conn.NoOp(); err == nil {
			return conn, nil
		}
		conn.Conn.Quit()
		delete(ftpConns, ftpURL)
	}

	host, user, pass, _, err := parseFTPURL(ftpURL)
	if err != nil {
		return nil, err
	}

	conn, err := ftp.Dial(host, ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("FTP dial error: %v", err)
	}

	err = conn.Login(user, pass)
	if err != nil {
		conn.Quit()
		return nil, fmt.Errorf("FTP login error: %v", err)
	}

	ftpConn := &FTPConnection{
		Conn: conn,
	}

	ftpConns[ftpURL] = ftpConn
	return ftpConn, nil
}

//________________________________________________________
//Force closes and removes FTP connection from cache
func forceCloseFTP(ftpURL string) {
	ftpMutex.Lock()
	defer ftpMutex.Unlock()
	if conn, exists := ftpConns[ftpURL]; exists {
		conn.Conn.Quit()
		delete(ftpConns, ftpURL)
	}
}

//________________________________________________________
//Upload wrapper with retry mechanism
func uploadToFTP(localPath, ftpURL string, rule SyncRule) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = doUploadToFTP(localPath, ftpURL, rule)
		if err == nil {
			return nil
		}
		fmt.Printf("[ERROR] FTP upload attempt %d failed: %v\n", attempt, err)
		forceCloseFTP(ftpURL)
		time.Sleep(1 * time.Second)
	}
	return err
}

//________________________________________________________
//Core upload file or directory to FTP logic
func doUploadToFTP(localPath, ftpURL string, rule SyncRule) error {
	ftpConn, err := getFTPConnection(ftpURL)
	if err != nil {
		return err
	}

	ftpConn.Mu.Lock()
	defer ftpConn.Mu.Unlock()

	host, _, _, ftpPath, err := parseFTPURL(ftpURL)
	if err != nil {
		return err
	}

	relPath := getTargetPath(localPath, rule)
	remotePath := filepath.ToSlash(filepath.Join(ftpPath, relPath))
	remotePath = strings.TrimPrefix(remotePath, "/")

	fi, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("Error stat local file: %v", err)
	}

	if fi.IsDir() {
		err = ftpConn.Conn.MakeDir(remotePath)
		if err != nil {
			// Suppressing MakeDir error, it usually means dir exists
		}
		logToFile("FTP_MKDIR", fmt.Sprintf("%s -> ftp://%s/%s", localPath, host, remotePath))
		return nil
	}

	dirs := strings.Split(filepath.ToSlash(filepath.Dir(remotePath)), "/")
	currentPath := ""
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		currentPath += "/" + dir
		ftpConn.Conn.MakeDir(currentPath)
	}

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("Error opening local file: %v", err)
	}
	defer file.Close()

	err = ftpConn.Conn.Stor(remotePath, file)
	if err != nil {
		return fmt.Errorf("FTP upload error: %v", err)
	}

	logToFile("FTP_UPLOAD", fmt.Sprintf("%s -> ftp://%s/%s", localPath, host, remotePath))
	return nil
}

//________________________________________________________
//Delete wrapper with retry mechanism
func deleteFromFTP(localPath, ftpURL string, rule SyncRule, isDir bool) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = doDeleteFromFTP(localPath, ftpURL, rule, isDir)
		if err == nil {
			return nil
		}
		fmt.Printf("[ERROR] FTP delete attempt %d failed: %v\n", attempt, err)
		forceCloseFTP(ftpURL)
		time.Sleep(1 * time.Second)
	}
	return err
}

//________________________________________________________
//Core delete file or directory from FTP logic
func doDeleteFromFTP(localPath, ftpURL string, rule SyncRule, isDir bool) error {
	ftpConn, err := getFTPConnection(ftpURL)
	if err != nil {
		return err
	}

	ftpConn.Mu.Lock()
	defer ftpConn.Mu.Unlock()

	host, _, _, ftpPath, err := parseFTPURL(ftpURL)
	if err != nil {
		return err
	}

	relPath := getTargetPath(localPath, rule)
	remotePath := filepath.ToSlash(filepath.Join(ftpPath, relPath))
	remotePath = strings.TrimPrefix(remotePath, "/")

	if isDir {
		err = removeFTPDirectory(ftpConn.Conn, remotePath)
		if err != nil {
			return fmt.Errorf("FTP delete directory error: %v", err)
		}
		logToFile("FTP_RMDIR", fmt.Sprintf("ftp://%s/%s", host, remotePath))
	} else {
		err = ftpConn.Conn.Delete(remotePath)
		if err != nil {
			return fmt.Errorf("FTP delete error: %v", err)
		}
		logToFile("FTP_DELETE", fmt.Sprintf("ftp://%s/%s", host, remotePath))
	}

	return nil
}

//________________________________________________________
//Recursively remove directory from FTP
func removeFTPDirectory(conn *ftp.ServerConn, path string) error {
	entries, err := conn.List(path)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		entryPath := filepath.ToSlash(filepath.Join(path, entry.Name))

		if entry.Type == ftp.EntryTypeFolder {
			err = removeFTPDirectory(conn, entryPath)
			if err != nil {
				return err
			}
		} else {
			err = conn.Delete(entryPath)
			if err != nil {
				return err
			}
		}
	}

	err = conn.RemoveDir(path)
	if err != nil {
		return err
	}

	return nil
}

//________________________________________________________
//Watch for changes in go-sync.txt
func watchSyncFile() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}

	syncFilePath := filepath.Join(filepath.Dir(exePath), "go-sync.txt")

	var lastModTime time.Time
	var lastSize int64

	if fi, err := os.Stat(syncFilePath); err == nil {
		lastModTime = fi.ModTime()
		lastSize = fi.Size()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		fi, err := os.Stat(syncFilePath)
		if err != nil {
			continue
		}

		if !fi.ModTime().Equal(lastModTime) || fi.Size() != lastSize {
			newRules := loadSyncRules()
			if len(newRules) > 0 || len(syncRules) > 0 {
				syncRulesMutex.Lock()
				syncRules = newRules
				syncRulesMutex.Unlock()
				fmt.Printf("\n[SYSTEM] Reloaded %d sync rules from go-sync.txt\n", len(syncRules))
			}

			lastModTime = fi.ModTime()
			lastSize = fi.Size()
		}
	}
}

//________________________________________________________
//Check if path matches pattern
func matchPattern(pattern, path string) bool {
	pattern = strings.ReplaceAll(pattern, "/", "\\")
	path = strings.ReplaceAll(path, "/", "\\")

	patternLower := strings.ToLower(pattern)
	pathLower := strings.ToLower(path)

	if strings.Contains(patternLower, "**") {
		parts := strings.Split(patternLower, "**")
		if len(parts) == 2 {
			return strings.HasPrefix(pathLower, parts[0]) && strings.HasSuffix(pathLower, parts[1])
		}
	}

	if strings.ContainsAny(patternLower, "*?") {
		matched, err := filepath.Match(patternLower, pathLower)
		if err == nil && matched {
			return true
		}

		_, fileName := filepath.Split(path)
		matched, err = filepath.Match(patternLower, strings.ToLower(fileName))
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
		basePattern = basePattern[:idx]
	} else if idx := strings.Index(basePattern, "*"); idx != -1 {
		basePattern = basePattern[:idx]
	}

	basePattern = strings.ReplaceAll(basePattern, "/", "\\")
	srcPathNorm := strings.ReplaceAll(srcPath, "/", "\\")

	relPath := ""
	if strings.HasPrefix(strings.ToLower(srcPathNorm), strings.ToLower(basePattern)) {
		relPath = srcPathNorm[len(basePattern):]
		relPath = strings.TrimPrefix(relPath, "\\")
	} else {
		relPath = filepath.Base(srcPath)
	}

	return relPath
}

//________________________________________________________
//Copy file using rule (preserving directory structure)
func copyFileWithRule(srcPath string, rule SyncRule) {
	relPath := getTargetPath(srcPath, rule)
	destPath := filepath.Join(rule.Target, relPath)

	fi, err := os.Stat(srcPath)
	if err != nil {
		return
	}

	if fi.IsDir() {
		os.MkdirAll(destPath, 0755)
		logToFile("MKDIR", destPath)
		return
	}

	os.MkdirAll(filepath.Dir(destPath), 0755)

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return
	}

	err = os.WriteFile(destPath, data, 0644)
	if err != nil {
		return
	}

	logToFile("COPY", fmt.Sprintf("%s -> %s", srcPath, destPath))
}

//________________________________________________________
//Execute sync rules for an event (safe to run concurrently)
func executeRules(filePath string, action uint32, isDir bool, oldPath string, oldIsDir bool) {
	syncRulesMutex.RLock()
	rulesCopy := make([]SyncRule, len(syncRules))
	copy(rulesCopy, syncRules)
	syncRulesMutex.RUnlock()

	for _, rule := range rulesCopy {
		if !matchPattern(rule.Pattern, filePath) {
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
				os.Remove(oldTarget)
				logToFile("DELETE", oldTarget)
			}

		case "sync":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				copyFileWithRule(filePath, rule)
			} else if action == windows.FILE_ACTION_REMOVED {
				relPath := getTargetPath(filePath, rule)
				targetPath := filepath.Join(rule.Target, relPath)
				if isDir {
					os.RemoveAll(targetPath)
				} else {
					os.Remove(targetPath)
				}
				logToFile("SYNC_DELETE", targetPath)
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				oldRelPath := getTargetPath(oldPath, rule)
				newRelPath := getTargetPath(filePath, rule)
				oldTarget := filepath.Join(rule.Target, oldRelPath)
				newTarget := filepath.Join(rule.Target, newRelPath)
				os.Rename(oldTarget, newTarget)
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

			command = strings.ReplaceAll(command, "$file", filepath.Base(filePath))
			command = strings.ReplaceAll(command, "$folder", filepath.Dir(filePath))
			command = strings.ReplaceAll(command, "$name", filepath.Base(filePath))
			command = strings.ReplaceAll(command, "$ext", filepath.Ext(filePath))

			eventType := "modify"
			switch action {
			case windows.FILE_ACTION_ADDED:
				eventType = "new"
			case windows.FILE_ACTION_REMOVED:
				eventType = "del"
			case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
				eventType = "rename"
			}
			command = strings.ReplaceAll(command, "$event", eventType)

			typeStr := "file"
			if isDir {
				typeStr = "dir"
			}
			command = strings.ReplaceAll(command, "$type", typeStr)

			logToFile("RUN_COMMAND", command)

			// Timeout for external commands to prevent blocking goroutines indefinitely
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			cmd := exec.CommandContext(ctx, "cmd", "/C", command)
			cmd.Dir = filepath.Dir(filePath)
			
			output, err := cmd.CombinedOutput()
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

	matcher := loadIgnoreRules()
	knownDirs := make(map[string]bool)

	drivePtr, err := windows.UTF16PtrFromString(drivePath)
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
	)
	if err != nil {
		fmt.Printf("[ERROR] Opening directory %s: %v\n", drivePath, err)
		return
	}
	defer windows.CloseHandle(handle)

	// Increased buffer size to 1MB to prevent ERROR_NOTIFY_ENUM_DIR during massive file changes
	buffer := make([]byte, 1024*1024)
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
		)
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

			info := (*windows.FileNotifyInformation)(unsafe.Pointer(&buffer[offset]))

			if info.FileNameLength == 0 || info.FileNameLength > 1024 {
				break
			}

			fileName := windows.UTF16ToString((*[1024]uint16)(unsafe.Pointer(&info.FileName))[0 : info.FileNameLength/2])

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

			fullPath := filepath.Join(drivePath, fileName)

			fi, statErr := os.Stat(fullPath)
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
					
					// Run executeRules concurrently so event loop doesn't block
					go executeRules(fullPath, info.Action, isDir, oldFullPath, oldIsDir)
					
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
				printColoredEvent(info.Action, isDir, msg)

				if info.Action != windows.FILE_ACTION_RENAMED_NEW_NAME {
					// Run executeRules concurrently so event loop doesn't block
					go executeRules(fullPath, info.Action, isDir, "", false)
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
	bitmask, err := windows.GetLogicalDrives()
	if err != nil {
		return drives
	}

	for i := 0; i < 26; i++ {
		if bitmask&(1<<i) != 0 {
			drive := string(rune('A'+i)) + ":\\"
			driveType := windows.GetDriveType(syscall.StringToUTF16Ptr(drive))
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
	ftpMutex.Lock()
	for url, conn := range ftpConns {
		conn.Conn.Quit()
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
			drivesToWatch = getLocalDrives()
		} else {
			arg = strings.TrimSuffix(arg, ":")
			arg = strings.TrimSuffix(arg, "\\")
			arg = strings.ToUpper(arg)
			drive := arg + ":\\"

			if _, err := os.Stat(drive); os.IsNotExist(err) {
				fmt.Printf("[ERROR] Drive %s not found.\n", drive)
				os.Exit(1)
			}
			drivesToWatch = append(drivesToWatch, drive)
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
		drivesToWatch = append(drivesToWatch, drive)
	}

	if len(drivesToWatch) == 0 {
		fmt.Println("[ERROR] No valid drives to watch found.")
		os.Exit(1)
	}

	syncRules = loadSyncRules()
	fmt.Printf("[SYSTEM] Loaded %d initial sync rules\n", len(syncRules))

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\n[SYSTEM] Shutting down...")
		onExit()
		os.Exit(0)
	}()

	go watchSyncFile()

	var wg sync.WaitGroup
	for _, drive := range drivesToWatch {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			startWatcher(d)
		}(drive)
	}

	fmt.Println("[SYSTEM] Application is running. Press Ctrl+C to exit.")
	wg.Wait()
}