package main

import (
	"log" // Logging package
	"os"  // OS operations
	"path/filepath" // Path manipulation
	"strings" // String operations

	"github.com/fsnotify/fsnotify" // File watcher library
	"github.com/gen2brain/beeep"     // Notification library
	"github.com/getlantern/systray"  // System tray library
)

//________________________________________________________
//Displays a temporary notification pop-up
func showNotification(title string, message string) {
	_ = beeep.Notify(title, message, "") // Show system notification banner
}

//________________________________________________________
//Adds directory and valid subdirectories to the watcher
func addRecursiveWatch(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error { // Walk directory tree
		if err != nil { // Handle access errors
			return filepath.SkipDir // Skip inaccessible folders completely
		} // End check

		if info.IsDir() { // Check if entry is a folder
			base := filepath.Base(path) // Get folder name
			if strings.HasPrefix(base, "$") || base == "System Volume Information" { // Match system folders
				return filepath.SkipDir // Skip folder traversal
			} // End check

			err = watcher.Add(path) // Add valid directory to watcher
			if err != nil { // Handle error on watch
				log.Println("Error watching path:", path, err) // Log error
			} // End check
		} // End check
		return nil // Continue walking
	})
}

//________________________________________________________
//Monitors disk changes in background
func startWatcher() {
	watcher, err := fsnotify.NewWatcher() // Create new watcher instance
	if err != nil { // Handle creation error
		log.Fatal(err) // Exit on failure
	} // End check
	defer watcher.Close() // Ensure watcher is closed on exit

	targetDir := "D:\\" // Target drive to monitor

	err = addRecursiveWatch(watcher, targetDir) // Add drive D and subfolders
	if err != nil { // Handle walking error
		log.Println("Error setting up recursive watch:", err) // Log error
	} // End check

	done := make(chan bool) // Control channel

	go func() { // Start event loop goroutine
		for { // Infinite loop
			select { // Channel selector
			case event, ok := <-watcher.Events: // Handle file event
				if !ok { // Check if channel is closed
					return // Exit loop
				} // End check

				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 { // Match target operations
					var action string // Action description variable

					switch { // Determine operation type
					case event.Op&fsnotify.Create != 0: // File or directory created
						action = "Создан" // Set created label
					case event.Op&fsnotify.Write != 0: // File content updated
						action = "Изменен" // Set updated label
					case event.Op&fsnotify.Remove != 0: // File or directory removed
						action = "Удален" // Set removed label
					case event.Op&fsnotify.Rename != 0: // File or directory renamed
						action = "Переименован" // Set renamed label
					} // End switch

					showNotification("Изменение на диске D:", action+": "+event.Name) // Trigger notification call

					if event.Op&fsnotify.Create != 0 { // Check if new item created
						fi, err := os.Stat(event.Name) // Inspect created item
						if err == nil && fi.IsDir() { // If created item is a folder
							_ = watcher.Add(event.Name) // Dynamically watch new directory
						} // End check
					} // End check
				} // End check

			case err, ok := <-watcher.Errors: // Handle error stream
				if !ok { // Check if channel is closed
					return // Exit loop
				} // End check
				log.Println("Watcher error:", err) // Log error message
			} // End select
		} // End loop
	}() // Execute goroutine

	<-done // Block goroutine
}

//________________________________________________________
//Initializes system tray icon and menu
func onReady() {
	systray.SetTitle("D: Watcher") // Set tray title
	systray.SetTooltip("Monitoring Drive D for changes") // Set mouse tooltip

	// Simple 16x16 red square icon bytes for initialization
	iconData := []byte{
		0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10,
		0x00, 0x00, 0x01, 0x00, 0x20, 0x00, 0x68, 0x04,
		0x00, 0x00, 0x16, 0x00, 0x00, 0x00,
	}
	systray.SetIcon(iconData) // Set tray icon

	mQuit := systray.AddMenuItem("Exit", "Quit application") // Add exit menu item

	go func() { // Start menu click handler
		<-mQuit.ClickedCh // Wait for quit click
		systray.Quit()    // Trigger tray exit
	}()

	go startWatcher() // Start file watching process
}

//________________________________________________________
//Executes cleanup when exiting tray mode
func onExit() {
	// Cleanup procedures if required
}


//________________________________________________________
//Main application entry point
func main() {
	systray.Run(onReady, onExit) // Run main tray event loop
}