package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

type app struct {
	cfg     Config
	cfgPath string
	tg      *telegram
	bark    *bark
	client  *http.Client
	cache   *idCache

	mu       sync.Mutex
	states   map[string]*svcState
	cancels  map[string]context.CancelFunc
	uiServer *http.Server
	cfgMu    sync.Mutex
	wg       sync.WaitGroup

	shutdownOnce sync.Once
	restartOnce  sync.Once
	restartCh    chan struct{}
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to config.json")
	printUIToken := flag.Bool("ui-token", false, "print local UI token and exit")
	testMode := flag.String("test", "", "validate vk, tg, max, bark or api (Bot API reachability) and exit")
	ctlMode := flag.String("ctl", "", "local control: status, control, send, debug")
	ctlService := flag.String("service", "", "service for -ctl control/send: vk, max, tg, bark")
	ctlAction := flag.String("action", "", "action for -ctl control")
	ctlValue := flag.String("value", "", "value for autostart")
	ctlText := flag.String("text", "", "message text for -ctl send")
	flag.Parse()

	uiTokenPath := filepath.Join(filepath.Dir(*configPath), "ui.token")
	if *printUIToken {
		tok, err := ensureUIToken(uiTokenPath)
		if err != nil {
			logf("ui token error: %v", err)
			os.Exit(1)
		}
		fmt.Println(tok)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logf("config error: %v", err)
		os.Exit(1)
	}
	initDebugLog(*configPath)
	debugLog.setSecrets(cfg)

	if *testMode != "" {
		runTest(cfg, *testMode)
		return
	}
	if *ctlMode != "" {
		runControl(*configPath, cfg, *ctlMode, *ctlService, *ctlAction, *ctlValue, *ctlText)
		return
	}

	// Only the long-running process restores logging. -ctl/test instances are
	// short-lived and must not append session headers on every poll.
	debugLog.restore()
	debugf("[APP] startup config=%s pid=%d", *configPath, os.Getpid())

	// Keep the heap small. Combined with the explicit frame/cache limits this
	// targets <15 MB RSS on Linux/arm64 with a statically linked binary.
	// GOGC 20 kept the heap tiny but pushed the runtime into continuous GC
	// near the memory limit. 50 is a better CPU/RSS trade-off on the router;
	// the hard limit still bounds the heap.
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(14 << 20)

	direct := &http.Client{Transport: baseTransport(), Timeout: 40 * time.Second, CheckRedirect: checkRedirect}

	// A bad proxy must not make the whole service unstartable: the UI/LuCI and
	// the other connections must stay available to fix the config.
	tgClient, tgErr := newTelegramHTTPClient(cfg)
	a := &app{
		cfg:       cfg,
		cfgPath:   *configPath,
		tg:        newTelegram(cfg, tgClient),
		bark:      newBark(cfg, direct),
		client:    direct,
		cache:     newIDCache(512),
		restartCh: make(chan struct{}),
	}
	initStates(a)
	if tgErr != nil {
		logf("telegram proxy error: %s", safeDetail(tgErr))
		a.tg.setEnabled(false)
		a.setState("tg", "error", "proxy: "+safeDetail(tgErr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Autostart is persisted per service; the buttons only change runtime state.
	if cfg.autostartVK() {
		a.start("vk")
	}
	if cfg.autostartMAX() {
		a.start("max")
	}
	if cfg.autostartTG() {
		a.start("tg")
	}
	if cfg.autostartBark() {
		a.start("bark")
	}

	// The local control API always runs: LuCI needs it for the status circles
	// and buttons, even when the full HTML page is disabled to save RAM.
	tok, err := ensureUIToken(uiTokenPath)
	if err != nil {
		logf("ui token error: %v", err)
	} else {
		go serveUI(a, *configPath, tok)
	}

	select {
	case <-ctx.Done():
	case <-a.restartCh:
		logf("restart запрошен из интерфейса")
	}
	logf("shutting down")
	debugf("[APP] shutdown")
	a.shutdown()
	// Keep the persistence marker so a restart starts logging again.
	debugLog.closeFile()
	closeIdleClient(direct)
	closeIdleClient(tgClient)
}
