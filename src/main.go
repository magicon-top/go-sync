package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	ignore "github.com/monochromegane/go-gitignore"
	"golang.org/x/sys/windows"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procFillRect         = user32.NewProc("FillRect")
	procDrawText         = user32.NewProc("DrawTextW")
	procGetClientRect    = user32.NewProc("GetClientRect")
	procLoadCursor       = user32.NewProc("LoadCursorW")
	procSetCursor        = user32.NewProc("SetCursor")

	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procSetBkMode        = gdi32.NewProc("SetBkMode")
	procSetTextColor     = gdi32.NewProc("SetTextColor")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procSelectObject     = gdi32.NewProc("SelectObject")
)

// Win32 Constants
const (
	WM_DESTROY      = 0x0002
	WM_PAINT        = 0x000F
	WM_ERASEBKGND   = 0x0014
	WM_SETCURSOR    = 0x0020
	WS_CHILD        = 0x40000000
	WS_VISIBLE      = 0x10000000
	WS_POPUP        = 0x80000000
	WS_THICKFRAME   = 0x00040000
	DT_SINGLELINE   = 0x0020
	DT_VCENTER      = 0x0004
	DT_NOPREFIX     = 0x0800
	DT_END_ELLIPSIS = 0x8000
	IDC_ARROW       = 32512
)

type RECT struct {
	Left, Top, Right, Bottom int32
}

type PAINTSTRUCT struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     RECT
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type MSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

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

var (
	overlayHwnd uintptr
	events      []EventLog
	eventsMutex = make(chan bool, 1)
	darkBgBrush uintptr
	knownDirs   = make(map[string]bool)
	arrowCursor uintptr
	syncRules   []SyncRule
)

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
		
		if strings.HasPrefix(action, "->") {
			rule.Action = "copy"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "->"))
		} else if strings.HasPrefix(action, "<>") {
			rule.Action = "sync"
			rule.Target = strings.TrimSpace(strings.TrimPrefix(action, "<>"))
		} else {
			rule.Action = "run"
			rule.Target = action
		}
		
		rules = append(rules, rule)
		fmt.Printf("Loaded rule: %s ; %s %s\n", pattern, rule.Action, rule.Target)
	}
	
	return rules
}

//________________________________________________________
//Watch for changes in go-sync.txt
func watchSyncFile() {
	fmt.Println("watchSyncFile started")
	
	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("Error getting executable path: %v\n", err)
		return
	}
	
	syncFilePath := filepath.Join(filepath.Dir(exePath), "go-sync.txt")
	fmt.Printf("Watching file: %s\n", syncFilePath)
	
	var lastModTime time.Time
	var lastSize int64
	
	if fi, err := os.Stat(syncFilePath); err == nil {
		lastModTime = fi.ModTime()
		lastSize = fi.Size()
		fmt.Printf("Initial mod time: %v, size: %d\n", lastModTime, lastSize)
	} else {
		fmt.Printf("Error getting file info: %v\n", err)
	}
	
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	
	for range ticker.C {
		fi, err := os.Stat(syncFilePath)
		if err != nil {
			fmt.Printf("Error stat file: %v\n", err)
			continue
		}
		
		if !fi.ModTime().Equal(lastModTime) || fi.Size() != lastSize {
			fmt.Printf("File changed! Old: %v (%d bytes), New: %v (%d bytes)\n", 
				lastModTime, lastSize, fi.ModTime(), fi.Size())
			
			newRules := loadSyncRules()
			if len(newRules) > 0 || len(syncRules) > 0 {
				syncRules = newRules
				fmt.Printf("Reloaded %d sync rules\n", len(syncRules))
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
	
	return filepath.Join(rule.Target, relPath)
}

//________________________________________________________
//Copy file using rule (preserving directory structure)
func copyFileWithRule(srcPath string, rule SyncRule) {
	destPath := getTargetPath(srcPath, rule)
	
	fmt.Printf("Copy: %s -> %s\n", srcPath, destPath)
	
	os.MkdirAll(filepath.Dir(destPath), 0755)
	
	data, err := os.ReadFile(srcPath)
	if err != nil {
		fmt.Printf("Error reading file: %v\n", err)
		return
	}
	
	err = os.WriteFile(destPath, data, 0644)
	if err != nil {
		fmt.Printf("Error writing file: %v\n", err)
		return
	}
	
	fmt.Printf("Copied: %s -> %s\n", srcPath, destPath)
}

//________________________________________________________
//Execute sync rules for an event
func executeRules(filePath string, action uint32, isDir bool, oldPath string) {
	fmt.Printf("Checking rules for: %s (action=%d)\n", filePath, action)
	
	for _, rule := range syncRules {
		if !matchPattern(rule.Pattern, filePath) {
			fmt.Printf("  No match: %s != %s\n", filePath, rule.Pattern)
			continue
		}
		
		fmt.Printf("  Rule matched: %s -> %s %s\n", rule.Pattern, rule.Action, rule.Target)
		
		switch rule.Action {
		case "copy":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				copyFileWithRule(filePath, rule)
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				copyFileWithRule(filePath, rule)
				oldTarget := getTargetPath(oldPath, rule)
				os.Remove(oldTarget)
			}
			
		case "sync":
			if action == windows.FILE_ACTION_ADDED || action == windows.FILE_ACTION_MODIFIED {
				copyFileWithRule(filePath, rule)
			} else if action == windows.FILE_ACTION_REMOVED {
				targetPath := getTargetPath(filePath, rule)
				os.Remove(targetPath)
				fmt.Printf("Sync deleted: %s\n", targetPath)
			} else if action == windows.FILE_ACTION_RENAMED_NEW_NAME && oldPath != "" {
				oldTarget := getTargetPath(oldPath, rule)
				newTarget := getTargetPath(filePath, rule)
				os.Rename(oldTarget, newTarget)
				fmt.Printf("Sync renamed: %s -> %s\n", oldTarget, newTarget)
			}
			
		case "run":
			command := rule.Target
			command = strings.ReplaceAll(command, "$file", filePath)
			command = strings.ReplaceAll(command, "$folder", filepath.Dir(filePath))
			command = strings.ReplaceAll(command, "$name", filepath.Base(filePath))
			command = strings.ReplaceAll(command, "$ext", filepath.Ext(filePath))
			
			eventType := "mod"
			switch action {
			case windows.FILE_ACTION_ADDED:
				eventType = "new"
			case windows.FILE_ACTION_REMOVED:
				eventType = "del"
			case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
				eventType = "ren"
			}
			command = strings.ReplaceAll(command, "$event", eventType)
			
			typeStr := "file"
			if isDir {
				typeStr = "dir"
			}
			command = strings.ReplaceAll(command, "$type", typeStr)
			
			fmt.Printf("Running command: %s\n", command)
			
			cmd := exec.Command("cmd", "/C", command)
			cmd.Dir = filepath.Dir(filePath)
			output, err := cmd.CombinedOutput()
			if err != nil {
				fmt.Printf("Command error: %v\n", err)
			}
			if len(output) > 0 {
				fmt.Printf("Command output: %s\n", string(output))
			}
		}
	}
}

//________________________________________________________
//Calculates length of null-terminated UTF-16 string
func utf16StrLen(s *uint16) int {
	p := s
	for *p != 0 {
		p = (*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 2))
	}
	return int(uintptr(unsafe.Pointer(p))-uintptr(unsafe.Pointer(s))) / 2
}

//________________________________________________________
//Window procedure for custom drawing
func WndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_SETCURSOR:
		if arrowCursor != 0 {
			procSetCursor.Call(arrowCursor)
			return 1
		}
		
	case WM_PAINT:
		var ps PAINTSTRUCT
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		
		var clientRect RECT
		procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&clientRect)))
		
		bgBrush, _, _ := procCreateSolidBrush.Call(uintptr(0x333333))
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&clientRect)), bgBrush)
		procDeleteObject.Call(bgBrush)
		
		fontName, _ := syscall.UTF16PtrFromString("Segoe UI")
		hFont, _, _ := procCreateFontW.Call(
			uintptr(14), 0, 0, 0,
			uintptr(400),
			0, 0, 0,
			uintptr(1),
			0, 0, 0, 0,
			uintptr(unsafe.Pointer(fontName)),
		)
		oldFont, _, _ := procSelectObject.Call(hdc, hFont)
		
		eventsMutex <- true
		
		yPos := int32(5)
		
		for i := 0; i < len(events) && yPos < clientRect.Bottom-20; i++ {
			ev := events[i]
			
			var fg, bg uint32
			defaultBg := uint32(0x333333)
			
			switch ev.ActionType {
			case windows.FILE_ACTION_ADDED:
				if ev.IsDir {
					fg = 0xFFFFFF
					bg = 0x006400
				} else {
					fg = 0x00FF7F
					bg = defaultBg
				}
				
			case windows.FILE_ACTION_REMOVED:
				if ev.IsDir {
					fg = 0xFFFFFF
					bg = 0x0000FF
				} else {
					fg = 0x0000FF
					bg = defaultBg
				}
				
			case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
				if ev.IsDir {
					fg = 0xFFFFFF
					bg = 0x0068D6
				} else {
					fg = 0x00A5FF
					bg = defaultBg
				}
				
			case windows.FILE_ACTION_MODIFIED:
				if ev.IsDir {
					fg = 0xFFFFFF
					bg = 0x8B0000
				} else {
					fg = 0xEBCE87
					bg = defaultBg
				}
				
			default:
				fg = 0xCCCCCC
				bg = defaultBg
			}
			
			itemRect := RECT{
				Left:   5,
				Top:    yPos,
				Right:  clientRect.Right - 5,
				Bottom: yPos + 20,
			}
			
			if ev.IsDir {
				itemBrush, _, _ := procCreateSolidBrush.Call(uintptr(bg))
				procFillRect.Call(hdc, uintptr(unsafe.Pointer(&itemRect)), itemBrush)
				procDeleteObject.Call(itemBrush)
			}
			
			procSetBkMode.Call(hdc, 1)
			procSetTextColor.Call(hdc, uintptr(fg))
			
			textPtr, _ := syscall.UTF16PtrFromString(ev.Text)
			textLen := utf16StrLen(textPtr)
			
			textRect := itemRect
			textRect.Left += 5
			
			procDrawText.Call(
				hdc,
				uintptr(unsafe.Pointer(textPtr)),
				uintptr(textLen),
				uintptr(unsafe.Pointer(&textRect)),
				DT_SINGLELINE|DT_VCENTER|DT_NOPREFIX|DT_END_ELLIPSIS,
			)
			
			yPos += 20
		}
		
		<-eventsMutex
		
		procSelectObject.Call(hdc, oldFont)
		procDeleteObject.Call(hFont)
		
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0

	case WM_ERASEBKGND:
		return 1

	case WM_DESTROY:
		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

//________________________________________________________
//Creates and runs the floating GUI message loop
func runOverlayWindow() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInstance, _, _ := procGetModuleHandle.Call(0)
	className, _ := syscall.UTF16PtrFromString("OverlayClass")

	darkBgBrush, _, _ = procCreateSolidBrush.Call(uintptr(0x333333))
	
	arrowCursor, _, _ = procLoadCursor.Call(0, uintptr(IDC_ARROW))

	wcex := WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEX{})),
		Style:         0,
		LpfnWndProc:   syscall.NewCallback(WndProc),
		HInstance:     hInstance,
		HCursor:       arrowCursor,
		HbrBackground: darkBgBrush,
		LpszClassName: className,
	}
	procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wcex)))

	winWidth := uintptr(300)
	winHeight := uintptr(200)

	screenWidth, _, _ := procGetSystemMetrics.Call(0)
	screenHeight, _, _ := procGetSystemMetrics.Call(1)

	xPos := screenWidth - winWidth - 15
	yPos := screenHeight - winHeight - 45

	overlayHwnd, _, _ = procCreateWindowEx.Call(
		uintptr(0x00000008|0x00000080|0x08000000),
		uintptr(unsafe.Pointer(className)),
		0,
		uintptr(WS_POPUP|WS_THICKFRAME|WS_VISIBLE),
		xPos, yPos, winWidth, winHeight,
		0, 0, hInstance, 0,
	)

	if overlayHwnd == 0 {
		return
	}

	procShowWindow.Call(overlayHwnd, 5)
	procUpdateWindow.Call(overlayHwnd)

	var msg MSG
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

//________________________________________________________
//Add event and refresh window
func addEvent(ev EventLog) {
	eventsMutex <- true
	events = append([]EventLog{ev}, events...)
	if len(events) > 50 {
		events = events[:50]
	}
	<-eventsMutex
	
	if overlayHwnd != 0 {
		procInvalidateRect.Call(overlayHwnd, 0, 1)
	}
}

//________________________________________________________
//Monitors drive D using native Windows API ReadDirectoryChangesW
func startWatcher() {
	fmt.Println("Watcher started")
	
	matcher := loadIgnoreRules()
	syncRules = loadSyncRules()
	
	fmt.Printf("Loaded %d sync rules\n", len(syncRules))
	
	go watchSyncFile()
	fmt.Println("Sync file watcher started")

	drivePtr, err := windows.UTF16PtrFromString("D:\\")
	if err != nil {
		fmt.Printf("Error creating UTF16 pointer: %v\n", err)
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
		fmt.Printf("Error opening directory: %v\n", err)
		return
	}
	defer windows.CloseHandle(handle)
	fmt.Println("Directory handle opened successfully")

	buffer := make([]byte, 65536)
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
			fmt.Printf("Error reading directory changes: %v\n", err)
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

			fullPath := filepath.Join("D:\\", fileName)

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
					delete(knownDirs, fullPath)
				}
				
			case windows.FILE_ACTION_RENAMED_OLD_NAME:
				oldFullPath = fullPath
				oldIsDir = isDir || knownDirs[fullPath]
				if oldIsDir {
					delete(knownDirs, fullPath)
				}
				
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
					executeRules(fullPath, info.Action, isDir, oldFullPath)
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
				addEvent(EventLog{
					Text:       msg,
					ActionType: info.Action,
					IsDir:      isDir,
				})
				
				if info.Action != windows.FILE_ACTION_RENAMED_NEW_NAME {
					executeRules(fullPath, info.Action, isDir, "")
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
//Initializes system tray icon and menu
func onReady() {
	systray.SetTitle("D: Watcher")
	systray.SetTooltip("Monitoring Drive D for changes")

	iconData := []byte{
		0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10,
		0x00, 0x00, 0x01, 0x00, 0x20, 0x00, 0x68, 0x04,
		0x00, 0x00, 0x16, 0x00, 0x00, 0x00,
	}
	systray.SetIcon(iconData)

	mQuit := systray.AddMenuItem("Exit", "Quit application")

	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	go runOverlayWindow()
	
	time.Sleep(500 * time.Millisecond)
	
	go startWatcher()
}

//________________________________________________________
//Executes cleanup when exiting tray mode
func onExit() {
	// Cleanup procedures if required
}

//________________________________________________________
//Main application entry point
func main() {
	systray.Run(onReady, onExit)
}