// winclipshot — let Windows terminals paste screenshots as file paths.
//
// Flow:
//   1. Watch the clipboard for new image data (OS event, no polling).
//   2. When an image arrives, inspect the currently-focused window.
//      - Terminal: save to a PNG file and replace the clipboard with the path.
//      - Other:    remember the image as "pending" and briefly watch for the
//                  user to switch focus to a terminal. If they do within the
//                  pending window, convert then. If not, the clipboard is
//                  never touched and a normal paste-as-image still works.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.design/x/clipboard"
)

var defaultTerminals = []string{
	"WindowsTerminal.exe",
	"wt.exe",
	"conhost.exe",
	"cmd.exe",
	"powershell.exe",
	"pwsh.exe",
	"alacritty.exe",
	"wezterm-gui.exe",
	"mintty.exe",
	"Hyper.exe",
	"Tabby.exe",
}

var (
	flagOutDir    = flag.String("out", "", "directory to save screenshots (default: %TEMP%\\winclipshot)")
	flagExtraTerm = flag.String("terminals", "", "additional terminal exe names, comma-separated (appended to defaults)")
	flagPending   = flag.Duration("pending", 60*time.Second, "how long to watch for terminal focus after a screenshot taken in another app")
	flagVerbose   = flag.Bool("v", false, "verbose logging")
	flagBench     = flag.Bool("bench", false, "run a micro-benchmark of the save+clipboard-write hot path and exit")
)

const pendingPollInterval = 200 * time.Millisecond

// pendingImage holds a screenshot that arrived while the foreground window
// was not a terminal. A goroutine polls foreground+clipboard until either
// the user focuses a terminal (→ convert), the clipboard changes to
// something else (→ abandon), or the window elapses (→ abandon).
type pendingState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

var pending pendingState

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime)

	if err := clipboard.Init(); err != nil {
		log.Fatalf("clipboard init failed: %v", err)
	}

	if *flagBench {
		runBenchmark()
		return
	}

	outDir := *flagOutDir
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "winclipshot")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatalf("create out dir: %v", err)
	}

	terms := map[string]bool{}
	for _, t := range defaultTerminals {
		terms[strings.ToLower(t)] = true
	}
	for _, t := range strings.Split(*flagExtraTerm, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			terms[strings.ToLower(t)] = true
		}
	}

	log.Printf("winclipshot running")
	log.Printf("  save dir     : %s", outDir)
	log.Printf("  terminals    : %s", strings.Join(sortedKeys(terms), ", "))
	log.Printf("  pending window: %s", *flagPending)
	log.Printf("take a screenshot (Win+Shift+S) as usual; pasting into a terminal yields the saved PNG path.")
	log.Printf("press Ctrl+C to quit.")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Printf("shutting down")
		cancel()
	}()

	ch := clipboard.Watch(ctx, clipboard.FmtImage)
	for png := range ch {
		if len(png) == 0 {
			continue
		}
		handleNewImage(png, outDir, terms)
	}
}

func handleNewImage(png []byte, outDir string, terms map[string]bool) {
	// Let focus settle after Snipping Tool's overlay closes.
	time.Sleep(150 * time.Millisecond)
	clearPending()

	proc := foregroundProcessName()
	if terms[strings.ToLower(proc)] {
		convertAndLog(png, outDir, proc, "immediate")
		return
	}

	startPending(png, outDir, terms, proc)
}

func startPending(png []byte, outDir string, terms map[string]bool, sourceProc string) {
	pending.mu.Lock()
	defer pending.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	pending.cancel = cancel

	if *flagVerbose {
		log.Printf("pending: image taken in %q; will convert if you focus a terminal within %s",
			orDash(sourceProc), *flagPending)
	}

	go watchPending(ctx, png, hashBytes(png), outDir, terms)
}

func clearPending() {
	pending.mu.Lock()
	if pending.cancel != nil {
		pending.cancel()
		pending.cancel = nil
	}
	pending.mu.Unlock()
}

func watchPending(ctx context.Context, png []byte, h uint64, outDir string, terms map[string]bool) {
	deadline := time.After(*flagPending)
	ticker := time.NewTicker(pendingPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			if *flagVerbose {
				log.Printf("pending: expired (%s)", *flagPending)
			}
			return
		case <-ticker.C:
			cur := clipboard.Read(clipboard.FmtImage)
			if len(cur) == 0 || hashBytes(cur) != h {
				if *flagVerbose {
					log.Printf("pending: clipboard changed, abandoning")
				}
				return
			}
			proc := foregroundProcessName()
			if terms[strings.ToLower(proc)] {
				convertAndLog(png, outDir, proc, "deferred")
				return
			}
		}
	}
}

func convertAndLog(png []byte, outDir, proc, mode string) {
	path := filepath.Join(outDir, fmt.Sprintf("clip-%s.png", time.Now().Format("20060102-150405.000")))
	if err := os.WriteFile(path, png, 0o644); err != nil {
		log.Printf("save image: %v", err)
		return
	}
	clipboard.Write(clipboard.FmtText, []byte(path))
	log.Printf("→ %s (paste in %s, %s)", path, proc, mode)
}

// --- benchmark ---------------------------------------------------------------

func runBenchmark() {
	// Use a realistic payload size: screenshot PNGs are typically a few hundred
	// KB. We fill with pseudo-random bytes so disk write cost is not skewed by
	// filesystem compression.
	const payloadBytes = 400 * 1024
	payload := make([]byte, payloadBytes)
	for i := range payload {
		payload[i] = byte(i * 1103515245)
	}

	tmp, err := os.MkdirTemp("", "winclipshot-bench-")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	const n = 200
	latencies := make([]time.Duration, 0, n)

	for i := 0; i < n; i++ {
		start := time.Now()
		path := filepath.Join(tmp, fmt.Sprintf("bench-%03d.png", i))
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			log.Fatalf("write: %v", err)
		}
		clipboard.Write(clipboard.FmtText, []byte(path))
		latencies = append(latencies, time.Since(start))
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p := func(q float64) time.Duration { return latencies[int(float64(n-1)*q)] }

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss := processWorkingSet()

	fmt.Printf("winclipshot micro-benchmark\n")
	fmt.Printf("  iterations     : %d\n", n)
	fmt.Printf("  payload size   : %d KiB\n", payloadBytes/1024)
	fmt.Printf("  save + clipboard.Write latency\n")
	fmt.Printf("     min          : %s\n", latencies[0])
	fmt.Printf("     p50          : %s\n", p(0.50))
	fmt.Printf("     p95          : %s\n", p(0.95))
	fmt.Printf("     p99          : %s\n", p(0.99))
	fmt.Printf("     max          : %s\n", latencies[n-1])
	fmt.Printf("  heap alloc     : %.2f MiB\n", float64(ms.HeapAlloc)/(1024*1024))
	fmt.Printf("  working set    : %.2f MiB\n", float64(rss)/(1024*1024))
	fmt.Printf("  goroutines     : %d\n", runtime.NumGoroutine())
}

// --- helpers -----------------------------------------------------------------

func hashBytes(b []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "<unknown>"
	}
	return s
}

// --- Win32 -------------------------------------------------------------------

var (
	user32                         = syscall.NewLazyDLL("user32.dll")
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	psapi                          = syscall.NewLazyDLL("psapi.dll")
	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId   = user32.NewProc("GetWindowThreadProcessId")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procGetCurrentProcess          = kernel32.NewProc("GetCurrentProcess")
	procGetProcessMemoryInfo       = psapi.NewProc("GetProcessMemoryInfo")
)

const processQueryLimitedInformation = 0x1000

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func processWorkingSet() uint64 {
	h, _, _ := procGetCurrentProcess.Call()
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	ret, _, _ := procGetProcessMemoryInfo.Call(h, uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if ret == 0 {
		return 0
	}
	return uint64(c.workingSetSize)
}

func foregroundProcessName() string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}
	h, _, _ := procOpenProcess.Call(uintptr(processQueryLimitedInformation), 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandle.Call(h)

	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	ret, _, _ := procQueryFullProcessImageNameW.Call(
		h, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0 {
		return ""
	}
	return filepath.Base(syscall.UTF16ToString(buf[:size]))
}
