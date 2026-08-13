// llama-swap-tray is the cross-platform system-tray helper for llama-swap
// (Windows and Linux; macOS uses the native Swift menu-bar app instead, but
// this binary also runs there for development).
//
// It is normally launched by llama-swap itself when `menu_bar` is enabled in
// the config file, receiving the backend address and bar selection via the
// LLAMA_SWAP_MENU_BASE_URL and LLAMA_SWAP_MENU_BARS environment variables.
// It can also run standalone against any llama-swap instance:
//
//	llama-swap-tray -base-url http://127.0.0.1:8080 -bars gpu,vram
//
// On Linux the tray requires a StatusNotifierItem/AppIndicator host (KDE,
// most desktops; GNOME needs the AppIndicator extension).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"

	"fyne.io/systray"

	"github.com/mostlygeek/llama-swap/internal/tray"
)

const maxModelItems = 32

func main() {
	flagBaseURL := flag.String("base-url", "", "llama-swap base URL (default: $LLAMA_SWAP_MENU_BASE_URL or http://127.0.0.1:8080)")
	flagBars := flag.String("bars", "", "comma-separated bar metrics: gpu, vram, cpu, ram (default: $LLAMA_SWAP_MENU_BARS or gpu,vram)")
	flag.Parse()

	client := tray.NewClient()
	if *flagBaseURL != "" {
		client.BaseURL = strings.TrimRight(*flagBaseURL, "/")
	}
	if *flagBars != "" {
		client.Bars = tray.ParseBars(*flagBars)
	}

	app := &trayApp{client: client}
	systray.Run(app.onReady, app.onExit)
}

type trayApp struct {
	client *tray.Client
	cancel context.CancelFunc

	mu         sync.Mutex
	modelItems []*systray.MenuItem
	modelIDs   []string

	completedItem *systray.MenuItem
	waitingItem   *systray.MenuItem
	loadItem      *systray.MenuItem
}

func (a *trayApp) onReady() {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	a.setIcon(tray.State{})
	systray.SetTooltip("llama-swap")

	a.completedItem = systray.AddMenuItem("0 completed", "requests completed")
	a.completedItem.Disable()
	a.waitingItem = systray.AddMenuItem("0 waiting", "requests waiting")
	a.waitingItem.Disable()
	a.loadItem = systray.AddMenuItem("load –", "current load")
	a.loadItem.Disable()

	systray.AddSeparator()

	// Pre-allocate hidden menu items for the model list: fyne systray can add
	// items at runtime but never remove them, so a fixed pool keeps the menu
	// stable across config reloads.
	a.modelItems = make([]*systray.MenuItem, maxModelItems)
	a.modelIDs = make([]string, maxModelItems)
	clickCases := make([]reflect.SelectCase, 0, maxModelItems+1)
	clickCases = append(clickCases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
	for i := range a.modelItems {
		item := systray.AddMenuItem("", "load this model")
		item.Hide()
		a.modelItems[i] = item
		clickCases = append(clickCases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(item.ClickedCh)})
	}
	// One dispatcher goroutine multiplexes all model-row clicks (case 0 is
	// ctx.Done()) instead of parking one goroutine per pool slot.
	go func() {
		for {
			chosen, _, _ := reflect.Select(clickCases)
			if chosen == 0 {
				return
			}
			a.mu.Lock()
			id := a.modelIDs[chosen-1]
			a.mu.Unlock()
			if id != "" {
				go a.client.LoadModel(ctx, id)
			}
		}
	}()

	systray.AddSeparator()
	unloadItem := systray.AddMenuItem("Unload All", "unload all running models")
	quitItem := systray.AddMenuItem("Quit", "quit the tray helper (llama-swap keeps running)")

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-unloadItem.ClickedCh:
				go a.client.UnloadAll(ctx)
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	a.client.OnChange = a.render
	go a.client.Run(ctx)
}

func (a *trayApp) onExit() {
	if a.cancel != nil {
		a.cancel()
	}
}

// render pushes a state snapshot into the tray UI. Called from the client's
// goroutines; systray calls are thread-safe.
func (a *trayApp) render(s tray.State) {
	a.setIcon(s)
	systray.SetTooltip(s.Tooltip(a.client.Bars))

	// Model name next to the icon where the platform supports it (macOS
	// always; Linux depends on the desktop environment; ignored on Windows).
	if runtime.GOOS != "windows" {
		systray.SetTitle(s.ActiveDisplayName())
	}

	a.completedItem.SetTitle(fmt.Sprintf("%d completed", s.Completed))
	// WaitingSummary breaks the count down per tier when more than one tier
	// is configured, and is otherwise identical to the plain "%d waiting"
	// this replaced - except it returns "" when nothing is waiting, so the
	// tray's original always-show-zero convention ("0 waiting" at startup and
	// whenever the queue drains) is preserved explicitly here.
	waitingTitle := s.WaitingSummary()
	if waitingTitle == "" {
		waitingTitle = fmt.Sprintf("%d waiting", s.Waiting)
	}
	a.waitingItem.SetTitle(waitingTitle)

	load := s.BarStrings(a.client.Bars)
	if !s.BackendOnline {
		load = append(load, "backend offline")
	}
	if len(load) > 0 {
		a.loadItem.SetTitle(strings.Join(load, " · "))
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for i, item := range a.modelItems {
		if i < len(s.Models) && i < maxModelItems {
			m := s.Models[i]
			a.modelIDs[i] = m.ID
			marker := "○ "
			if m.ID == s.ActiveModelID {
				marker = "● "
			}
			item.SetTitle(marker + m.DisplayName())
			item.Show()
		} else {
			a.modelIDs[i] = ""
			item.Hide()
		}
	}
	if len(s.Models) > maxModelItems {
		fmt.Fprintf(os.Stderr, "llama-swap-tray: %d models exceed the %d menu slots; extras hidden\n", len(s.Models), maxModelItems)
	}
}

func (a *trayApp) setIcon(s tray.State) {
	if runtime.GOOS == "windows" {
		systray.SetIcon(tray.RenderIconICO(s.BarValues))
	} else {
		systray.SetIcon(tray.RenderIconPNG(s.BarValues))
	}
}
