//go:build windows

package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

const (
	wmDestroy       = 0x0002
	wmMove          = 0x0003
	wmSize          = 0x0005
	wmSetFocus      = 0x0007
	wmClose         = 0x0010
	wmGetMinMaxInfo = 0x0024
	wmMoving        = 0x0216
	wmApp           = 0x8000

	wsOverlappedWindow = 0x00CF0000
	swShow             = 5
	sizeMinimized      = 1
	smCxScreen         = 0
	smCyScreen         = 1
	smCxIcon           = 11
	smCyIcon           = 12
	smCxSmIcon         = 49
	smCySmIcon         = 50
	idcArrow           = 32512
	imageIcon          = 1

	mbOK              = 0x00000000
	mbYesNo           = 0x00000004
	mbIconError       = 0x00000010
	mbIconWarning     = 0x00000030
	mbIconInformation = 0x00000040
	mbDefButton2      = 0x00000100
	mbSetForeground   = 0x00010000
	idYes             = 6

	minWidth  = 760
	minHeight = 560

	// The solder-mask colour of the page, as a COLORREF (0x00BBGGRR), so the
	// window does not flash white before the page paints.
	boardColor = 0x000F110B
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procLoadImageW       = user32.NewProc("LoadImageW")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procGetDpiForSystem  = user32.NewProc("GetDpiForSystem")
	procSetDpiContext    = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetDPIAware      = user32.NewProc("SetProcessDPIAware")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
)

var procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

// Without this Windows draws the window at 96 DPI and stretches the bitmap,
// which blurs text on scaled displays. System awareness renders at the
// primary display's scale and needs no handling of per-monitor changes.
// It has to happen before the first window, so it runs at start-up.
func init() {
	const systemAware = ^uintptr(1) // DPI_AWARENESS_CONTEXT_SYSTEM_AWARE (-2)
	if procSetDpiContext.Find() == nil {
		if ok, _, _ := procSetDpiContext.Call(systemAware); ok != 0 {
			return
		}
	}
	if procSetDPIAware.Find() == nil {
		procSetDPIAware.Call()
	}
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type point struct{ x, y int32 }

type winMsg struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type minMaxInfo struct {
	reserved     point
	maxSize      point
	maxPosition  point
	minTrackSize point
	maxTrackSize point
}

// WebViewVersion returns the version of the installed WebView2 runtime, or
// "" when there is none.
func WebViewVersion() string {
	v, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString("")
	if err != nil {
		return ""
	}
	return v
}

type window struct {
	hwnd     uintptr
	chromium *edge.Chromium
	ready    bool
	scale    float64
	handler  Handler

	mu    sync.Mutex
	queue []func()
}

// There is one window per process; the window procedure finds it here.
var active *window

// RunWindow shows the application window and blocks until it is closed. It
// must run on the main thread, which has to be locked to its goroutine.
func RunWindow(o WindowOptions) error {
	if active != nil {
		return errors.New("the application window is already open")
	}
	if o.DataDir != "" {
		if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", o.DataDir, err)
		}
	}
	w := &window{handler: o.Handler, scale: 1}
	if procGetDpiForSystem.Find() == nil {
		if dpi, _, _ := procGetDpiForSystem.Call(); dpi >= 96 {
			w.scale = float64(dpi) / 96
		}
	}
	active = w
	defer func() { active = nil }()

	var instance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		return fmt.Errorf("GetModuleHandle: %w", err)
	}
	className, _ := windows.UTF16PtrFromString("uefimgrWindow")
	title, _ := windows.UTF16PtrFromString(o.Title)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	brush, _, _ := procCreateSolidBrush.Call(boardColor)

	wc := wndClassEx{
		wndProc:    windows.NewCallback(wndProc),
		instance:   instance,
		icon:       loadIcon(instance, smCxIcon, smCyIcon),
		cursor:     windows.Handle(cursor),
		background: windows.Handle(brush),
		className:  className,
		iconSm:     loadIcon(instance, smCxSmIcon, smCySmIcon),
	}
	wc.size = uint32(unsafe.Sizeof(wc))
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return fmt.Errorf("RegisterClassEx: %w", err)
	}

	width, height := w.px(o.Width), w.px(o.Height)
	screenW, _, _ := procGetSystemMetrics.Call(smCxScreen)
	screenH, _, _ := procGetSystemMetrics.Call(smCyScreen)
	if int(screenW) > 0 && width > int(screenW) {
		width = int(screenW)
	}
	if int(screenH) > 0 && height > int(screenH) {
		height = int(screenH)
	}
	x, y := (int(screenW)-width)/2, (int(screenH)-height)/2
	if x < 0 || y < 0 {
		x, y = 0, 0
	}
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		0, 0, uintptr(instance), 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	w.hwnd = hwnd
	procShowWindow.Call(hwnd, swShow)
	// Windows grants this only to a process the user just started, which an
	// elevated launch from Explorer is. Otherwise the taskbar button flashes.
	procSetForegroundWindow.Call(hwnd)
	procUpdateWindow.Call(hwnd)

	chromium := edge.NewChromium()
	chromium.DataPath = o.DataDir
	chromium.Debug = o.Debug
	// The library exits the process after reporting an error, and never
	// returns from Embed if the browser cannot start. Say why before going.
	chromium.SetErrorCallback(func(err error) {
		Error(o.Title, "The application window could not be created:\n\n"+err.Error()+
			"\n\nStart uefimgr with --browser to use your web browser instead.")
		os.Exit(1)
	})
	chromium.MessageCallback = func(message string, _ *edge.ICoreWebView2, _ *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		w.receive(message)
	}
	w.chromium = chromium
	chromium.Embed(hwnd)
	chromium.Resize()

	if settings, err := chromium.GetSettings(); err == nil {
		_ = settings.PutAreDefaultContextMenusEnabled(o.Debug)
		_ = settings.PutAreDevToolsEnabled(o.Debug)
		_ = settings.PutIsStatusBarEnabled(false)
		_ = settings.PutIsZoomControlEnabled(o.Debug)
	}
	if c2 := chromium.GetController().GetICoreWebView2Controller2(); c2 != nil {
		_ = c2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 255, R: 0x0b, G: 0x11, B: 0x0f})
	}
	chromium.NavigateToString(withBridge(o.HTML))
	w.ready = true
	w.focus()

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}

func (w *window) px(logical int) int { return int(float64(logical) * w.scale) }

// loadIcon picks the image of the executable's APP icon resource that fits
// the system size given by the two metrics. It returns 0, the default icon,
// when the resource is missing.
func loadIcon(instance windows.Handle, cxMetric, cyMetric uintptr) windows.Handle {
	name, _ := windows.UTF16PtrFromString("APP")
	cx, _, _ := procGetSystemMetrics.Call(cxMetric)
	cy, _, _ := procGetSystemMetrics.Call(cyMetric)
	h, _, _ := procLoadImageW.Call(uintptr(instance), uintptr(unsafe.Pointer(name)), imageIcon, cx, cy, 0)
	return windows.Handle(h)
}

// receive handles one request from the page. The handler runs off the UI
// thread; its answer is posted back to it.
func (w *window) receive(message string) {
	var req bridgeRequest
	if err := json.Unmarshal([]byte(message), &req); err != nil || req.Method == "" {
		return
	}
	go func() {
		value, err := w.handler(req.Method, req.Params)
		script := settleScript(req.ID, value, err)
		w.dispatch(func() { w.chromium.Eval(script) })
	}()
}

// dispatch runs fn on the UI thread.
func (w *window) dispatch(fn func()) {
	w.mu.Lock()
	w.queue = append(w.queue, fn)
	w.mu.Unlock()
	procPostMessageW.Call(w.hwnd, wmApp, 0, 0)
}

func (w *window) drain() {
	w.mu.Lock()
	queue := w.queue
	w.queue = nil
	w.mu.Unlock()
	for _, fn := range queue {
		fn()
	}
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	w := active
	if w != nil && (w.hwnd == 0 || w.hwnd == hwnd) {
		switch message {
		case wmSize:
			if w.ready && wParam != sizeMinimized {
				w.chromium.Resize()
			}
		case wmMove, wmMoving:
			if w.ready {
				_ = w.chromium.NotifyParentWindowPositionChanged()
			}
		case wmSetFocus:
			w.focus()
		case wmGetMinMaxInfo:
			// lParam carries a pointer the system owns. Reading it through
			// its own address converts it without a uintptr round trip the
			// garbage collector could misread.
			info := (*minMaxInfo)(*(*unsafe.Pointer)(unsafe.Pointer(&lParam)))
			info.minTrackSize = point{int32(w.px(minWidth)), int32(w.px(minHeight))}
			return 0
		case wmApp:
			w.drain()
			return 0
		case wmClose:
			if w.chromium != nil {
				w.chromium.ShuttingDown()
			}
			w.ready = false
			procDestroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			procPostQuitMessage.Call(0)
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

// focus hands keyboard focus to the page. A refusal, for example while the
// window is minimized, is not worth reporting.
func (w *window) focus() {
	if !w.ready {
		return
	}
	if c := w.chromium.GetController(); c != nil {
		_ = c.MoveFocus(edge.COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC)
	}
}

func messageBox(title, text string, flags uint32) int32 {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	ret, _ := windows.MessageBox(0, t, c, flags|mbSetForeground)
	return ret
}

// Info shows a message and waits for it to be dismissed.
func Info(title, text string) { messageBox(title, text, mbOK|mbIconInformation) }

// Error shows an error message and waits for it to be dismissed.
func Error(title, text string) { messageBox(title, text, mbOK|mbIconError) }

// Confirm asks a yes/no question; "No" is the default answer.
func Confirm(title, text string) bool {
	return messageBox(title, text, mbYesNo|mbIconWarning|mbDefButton2) == idYes
}

// explorer starts the shell with one argument. The running shell takes the
// request over, so what it opens runs as the desktop user even though this
// process is elevated.
func explorer(arg string) error {
	dir, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		return err
	}
	return exec.Command(filepath.Join(dir, "explorer.exe"), arg).Start()
}

// OpenURL opens a web address in the default browser.
func OpenURL(url string) error { return explorer(url) }

// OpenFolder shows a folder in File Explorer.
func OpenFolder(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return explorer(path)
}
