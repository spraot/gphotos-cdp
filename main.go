/*
Copyright 2019 The Perkeep Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The gphotos-cdp program uses the Chrome DevTools Protocol to drive a Chrome session
// that downloads your photos stored in Google Photos.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evilsocket/islazy/zip"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	nItemsFlag   = flag.Int("n", -1, "number of items to download. If negative, get them all.")
	devFlag      = flag.Bool("dev", false, "dev mode. we reuse the same session dir (/tmp/gphotos-cdp), so we don't have to auth at every run.")
	dlDirFlag    = flag.String("dldir", "", "where to write the downloads. defaults to $HOME/Downloads/gphotos-cdp.")
	startFlag    = flag.String("start", "", "skip all photos until this location is reached. for debugging.")
	runFlag      = flag.String("run", "", "the program to run on each downloaded item, right after it is dowloaded. It is also the responsibility of that program to remove the downloaded item, if desired.")
	verboseFlag  = flag.Bool("v", false, "be verbose")
	fileDateFlag = flag.Bool("date", false, "set the file date to the photo date from the Google Photos UI")
	headlessFlag = flag.Bool("headless", false, "Start chrome browser in headless mode (cannot do authentication this way).")
	jsonLogFlag  = flag.Bool("json", false, "output logs in JSON format")
	logLevelFlag = flag.String("loglevel", "", "log level: debug, info, warn, error, fatal, panic")
	fixFlag      = flag.Bool("fix", false, "instead of skipping already downloaded files, check if they have the correct filename, date, and size")
	lastDoneFlag = flag.String("lastdone", ".lastdone", "name of file to store last done URL in (in dlDir)")
	workersFlag  = flag.Int("workers", 10, "number of concurrent downloads allowed")
	albumIdFlag  = flag.String("album", "", "ID of album to download, has no effect if lastdone file is populated")
)

var tick = 500 * time.Millisecond
var errStillProcessing = errors.New("video is still processing & can be downloaded later")
var errRetry = errors.New("retry")
var errNoDownloadButton = errors.New("no download button found")
var originalSuffix = "_original"

func main() {
	zerolog.TimestampFieldName = "dt"
	zerolog.TimeFieldFormat = "2006-01-02T15:04:05.999Z07:00"
	flag.Parse()
	if *nItemsFlag == 0 {
		return
	}
	if *verboseFlag && *logLevelFlag == "" {
		*logLevelFlag = "debug"
	}
	level, err := zerolog.ParseLevel(*logLevelFlag)
	if err != nil {
		log.Fatal().Err(err).Msgf("-loglevel argument not valid")
	}
	zerolog.SetGlobalLevel(level)
	if !*jsonLogFlag {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
	}
	if !*devFlag && *startFlag != "" {
		log.Fatal().Msg("-start only allowed in dev mode")
	}
	if !*devFlag && *headlessFlag {
		log.Fatal().Msg("-headless only allowed in dev mode")
	}

	// Set XDG_CONFIG_HOME and XDG_CACHE_HOME to a temp dir to solve issue in newer versions of Chromium
	if os.Getenv("XDG_CONFIG_HOME") == "" {
		if err := os.Setenv("XDG_CONFIG_HOME", filepath.Join(os.TempDir(), ".chromium")); err != nil {
			log.Fatal().Msgf("err %v", err)
		}
	}
	if os.Getenv("XDG_CACHE_HOME") == "" {
		if err := os.Setenv("XDG_CACHE_HOME", filepath.Join(os.TempDir(), ".chromium")); err != nil {
			log.Fatal().Msgf("err %v", err)
		}
	}

	s, err := NewSession()
	if err != nil {
		log.Err(err).Msgf("Failed to create session")
		return
	}
	defer s.Shutdown()

	log.Info().Msgf("Session Dir: %v", s.profileDir)

	if err := s.cleanDlDir(); err != nil {
		log.Err(err).Msgf("Failed to clean download directory %v", s.dlDir)
		return
	}

	ctx, cancel := s.NewContext()
	defer cancel()

	if err := s.login(ctx); err != nil {
		log.Err(err).Msg("login failed")
		return
	}

	if err := s.checkLocale(ctx); err != nil {
		log.Err(err).Msg("checking the locale failed")
		return
	}

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(s.resync()),
		// chromedp.ActionFunc(s.firstNav),
		// chromedp.ActionFunc(func(ctx context.Context) error {
		// 	var location string
		// 	if err := chromedp.Location(&location).Do(ctx); err != nil {
		// 		return err
		// 	}
		// 	log.Debug().Msgf("Location: %v", location)
		// 	return nil
		// }),
		// chromedp.ActionFunc(s.navN(*nItemsFlag)),
	); err != nil {
		log.Fatal().Msg(err.Error())
	}

	log.Info().Msg("Done")
}

type PhotoData struct {
	date     time.Time
	filename string
	fileSize int64
}

type Job struct {
	location string
	errChan  chan error
}

type NewDownload struct {
	GUID              string
	suggestedFilename string
}

type DownloadChannels struct {
	newdl    chan NewDownload
	progress chan bool
}

type Session struct {
	parentContext context.Context
	parentCancel  context.CancelFunc
	dlDir         string // dir where the photos get stored
	dlDirTmp      string // dir where the photos get stored temporarily
	profileDir    string // user data session dir. automatically created on chrome startup.
	// lastDone is the most recent (wrt to Google Photos timeline) item (its URL
	// really) that was downloaded. If set, it is used as a sentinel, to indicate that
	// we should skip dowloading all items older than this one.
	lastDone string
	nextDl   chan DownloadChannels
	err      chan error
}

// getLastDone returns the URL of the most recent item that was downloaded in
// the previous run. If any, it should have been stored in dlDir/{*lastDoneFlag}
func getLastDone(dlDir string) (string, error) {
	fn := filepath.Join(dlDir, *lastDoneFlag)
	data, err := os.ReadFile(fn)
	if os.IsNotExist(err) {
		log.Info().Msgf("No last done file (%v) found in %v", *lastDoneFlag, dlDir)
		return "", nil
	}
	if err != nil {
		return "", err
	}
	log.Debug().Msgf("Read last done file (%v) from %v: %v", *lastDoneFlag, fn, string(data))
	return string(data), nil
}

func NewSession() (*Session, error) {
	var dir string
	if *devFlag {
		dir = filepath.Join(os.TempDir(), "gphotos-cdp")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	} else {
		var err error
		dir, err = os.MkdirTemp("", "gphotos-cdp")
		if err != nil {
			return nil, err
		}
	}
	dlDir := *dlDirFlag
	if dlDir == "" {
		dlDir = filepath.Join(os.Getenv("HOME"), "Downloads", "gphotos-cdp")
	}
	if err := os.MkdirAll(dlDir, 0700); err != nil {
		return nil, err
	}
	dlDirTmp := filepath.Join(dlDir, "tmp")
	if err := os.MkdirAll(dlDirTmp, 0700); err != nil {
		return nil, err
	}
	lastDone, err := getLastDone(dlDir)
	if err != nil {
		return nil, err
	}
	s := &Session{
		profileDir: dir,
		dlDir:      dlDir,
		dlDirTmp:   dlDirTmp,
		lastDone:   lastDone,
		nextDl:     make(chan DownloadChannels, 1),
		err:        make(chan error, 1),
	}
	return s, nil
}

func (s *Session) NewContext() (context.Context, context.CancelFunc) {
	log.Info().Msgf("Starting Chrome browser")

	// Let's use as a base for allocator options (It implies Headless)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.UserDataDir(s.profileDir),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", "en-US,en"),
		chromedp.Flag("accept-lang", "en-US,en"),
	)

	if !*headlessFlag {
		// undo the three opts in chromedp.Headless() which is included in DefaultExecAllocatorOptions
		opts = append(opts, chromedp.Flag("headless", false))
		opts = append(opts, chromedp.Flag("hide-scrollbars", false))
		opts = append(opts, chromedp.Flag("mute-audio", false))
		// undo DisableGPU from above
		opts = append(opts, chromedp.Flag("disable-gpu", false))
	}
	ctx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	s.parentContext = ctx
	s.parentCancel = cancel

	ctx, cancel = chromedp.NewContext(s.parentContext)
	ctx = SetContextLocks(ctx)
	// browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).WithDownloadPath(s.dlDirTmp).WithEventsEnabled(true).Do(ctx)
	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			c := chromedp.FromContext(ctx)
			return browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).WithDownloadPath(s.dlDirTmp).WithEventsEnabled(true).
				// use the Browser executor so that it does not pass "sessionId" to the command.
				Do(cdp.WithExecutor(ctx, c.Browser))
		}),
	); err != nil {
		panic(err)
	}

	startDlListener(ctx, s.nextDl, s.err)

	return ctx, cancel
}

func (s *Session) Shutdown() {
	s.parentCancel()
}

// cleanDlDir removes all files (but not directories) from s.dlDir
func (s *Session) cleanDlDir() error {
	if s.dlDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dlDirTmp)
	if err != nil {
		return err
	}
	for _, v := range entries {
		if v.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(s.dlDirTmp, v.Name())); err != nil {
			return err
		}
	}
	return nil
}

// login navigates to https://photos.google.com/ and waits for the user to have
// authenticated (or for 2 minutes to have elapsed).
func (s *Session) login(ctx context.Context) error {
	log.Info().Msg("Starting authentication...")
	return chromedp.Run(ctx,
		chromedp.Navigate("https://photos.google.com/"),
		// when we're not authenticated, the URL is actually
		// https://www.google.com/photos/about/ , so we rely on that to detect when we have
		// authenticated.
		chromedp.ActionFunc(func(ctx context.Context) error {
			tick := time.Second
			timeout := time.Now().Add(2 * time.Minute)
			var location string
			for {
				if time.Now().After(timeout) {
					return errors.New("timeout waiting for authentication")
				}
				if err := chromedp.Location(&location).Do(ctx); err != nil {
					return err
				}
				if strings.HasPrefix(location, "https://photos.google.com") {
					return nil
				}
				if *headlessFlag {
					dlScreenshot(ctx, filepath.Join(s.dlDir, "error.png"))
					return errors.New("authentication not possible in -headless mode, see error.png (at " + location + ")")
				}
				log.Debug().Msgf("Not yet authenticated, at: %v", location)
				time.Sleep(tick)
			}
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			log.Info().Msg("Successfully authenticated")
			return nil
		}),
	)
}

func (s *Session) checkLocale(ctx context.Context) error {
	var locale string

	err := chromedp.Run(ctx,
		chromedp.EvaluateAsDevTools(`
				(function() {
					// Try to get locale from html lang attribute
					const htmlLang = document.documentElement.lang;
					if (htmlLang) return htmlLang;
					
					// Try to get locale from meta tags
					const metaLang = document.querySelector('meta[property="og:locale"]');
					if (metaLang) return metaLang.content;
					
					// Try to get locale from Google's internal data
					const scripts = document.getElementsByTagName('script');
					for (const script of scripts) {
						if (script.text && script.text.includes('"locale"')) {
							const match = script.text.match(/"locale":\s*"([^"]+)"/);
							if (match) return match[1];
						}
					}
					
					return "unknown";
				})()
			`, &locale),
	)

	if err != nil {
		log.Warn().Err(err).Msg("Failed to detect account locale")
	} else if !strings.HasPrefix(locale, "en") {
		log.Warn().Msgf("Detected Google account locale %v, this is likely to cause issues. Please change account language to English (en)", locale)
	}

	return nil
}

func dlScreenshot(ctx context.Context, filePath string) {
	var buf []byte

	log.Trace().Msgf("Saving screenshot to %v", filePath+".png")
	if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		log.Err(err).Msg(err.Error())
	} else if err := os.WriteFile(filePath+".png", buf, os.FileMode(0666)); err != nil {
		log.Err(err).Msg(err.Error())
	}

	// Dump the HTML to a file
	var html string
	if err := chromedp.Run(ctx, chromedp.OuterHTML("html", &html, chromedp.ByQuery)); err != nil {
		log.Err(err).Msg(err.Error())
	} else if err := os.WriteFile(filePath+".html", []byte(html), 0640); err != nil {
		log.Err(err).Msg(err.Error())
	}
}

// firstNav does either of:
// 1) if a specific photo URL was specified with *startFlag, it navigates to it
// 2) if the last session marked what was the most recent downloaded photo, it navigates to it
// 3) otherwise it jumps to the end of the timeline (i.e. the oldest photo)
func (s *Session) firstNav(ctx context.Context) (err error) {
	// This is only used to ensure page is loaded
	if err := s.setFirstItem(ctx); err != nil {
		return err
	}

	if *startFlag != "" {
		// TODO(mpl): use RunResponse
		chromedp.Navigate(*startFlag).Do(ctx)
		chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
		return nil
	}

	relPath := ""
	if *albumIdFlag != "" {
		relPath = "album/" + *albumIdFlag
	}

	if s.lastDone != "" {
		resp, err := chromedp.RunResponse(ctx, chromedp.Navigate(s.lastDone))
		if err != nil {
			return err
		}
		if resp.Status == http.StatusOK {
			chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
			log.Info().Msgf("Successfully navigated back to last done item: %s", s.lastDone)
			return nil
		}
		lastDoneFile := filepath.Join(s.dlDir, *lastDoneFlag)
		log.Info().Msgf("%s does not seem to exist anymore. Removing %s.", s.lastDone, lastDoneFile)
		s.lastDone = ""
		if err := os.Remove(lastDoneFile); err != nil {
			if os.IsNotExist(err) {
				log.Err(err).Msgf("Failed to remove %v file because it was already gone.", lastDoneFile)
			}
			return err
		}
	}

	// restart from scratch
	resp, err := chromedp.RunResponse(ctx, chromedp.Navigate("https://photos.google.com/"+relPath))
	if err != nil {
		return err
	}
	code := resp.Status
	if code != http.StatusOK {
		return fmt.Errorf("unexpected %d code when restarting to https://photos.google.com/%s", code, relPath)
	}
	chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)

	log.Debug().Msg("Finding end of page")

	if err := s.navToEnd(ctx); err != nil {
		return err
	}

	if err := s.navToLast(ctx); err != nil {
		return err
	}

	return nil
}

// setFirstItem looks for the first item, and sets it as s.firstItem.
// We always run it first even for code paths that might not need s.firstItem,
// because we also run it for the side-effect of waiting for the first page load to
// be done, and to be ready to receive scroll key events.
func (s *Session) setFirstItem(ctx context.Context) error {
	// wait for page to be loaded, i.e. that we can make an element active by using
	// the right arrow key.
	var firstItem string
	for {
		log.Trace().Msg("Attempting to find first item")
		attributes := make(map[string]string)
		if err := chromedp.Run(ctx,
			chromedp.KeyEvent(kb.ArrowRight),
			chromedp.Sleep(tick),
			chromedp.Attributes(`document.activeElement`, &attributes, chromedp.ByJSPath)); err != nil {
			return err
		}
		if len(attributes) == 0 {
			time.Sleep(tick)
			continue
		}

		photoHref, ok := attributes["href"]
		if !ok || !strings.HasPrefix(photoHref, "./photo/") {
			time.Sleep(tick)
			continue
		}

		firstItem = strings.TrimPrefix(photoHref, "./photo/")
		break
	}
	log.Debug().Msgf("Page loaded, most recent item in the feed is: %s", firstItem)
	return nil
}

// navToEnd scrolls down to the end of the page, i.e. to the oldest items.
func (s *Session) navToEnd(ctx context.Context) error {
	// try jumping to the end of the page. detect we are there and have stopped
	// moving when two consecutive screenshots are identical.
	var previousScr, scr []byte
	for {
		if err := chromedp.Run(ctx,
			chromedp.KeyEvent(kb.PageDown),
			chromedp.KeyEvent(kb.End),
			chromedp.Sleep(tick*time.Duration(5)),
			chromedp.CaptureScreenshot(&scr),
		); err != nil {
			return err
		}
		if previousScr == nil {
			previousScr = scr
			continue
		}
		if bytes.Equal(previousScr, scr) {
			break
		}
		previousScr = scr
	}

	log.Debug().Msg("Successfully jumped to the end")

	return nil
}

// navToLast sends the "\n" event until we detect that an item is loaded as a
// new page. It then sends the right arrow key event until we've reached the very
// last item.
func (s *Session) navToLast(ctx context.Context) error {
	deadline := time.Now().Add(4 * time.Minute)
	var location, prevLocation string
	ready := false
	for {
		// Check if context canceled
		if time.Now().After(deadline) {
			dlScreenshot(ctx, filepath.Join(s.dlDir, "error"))
			return errors.New("timed out while finding last photo, see error.png")
		}

		chromedp.KeyEvent(kb.ArrowRight).Do(ctx)
		time.Sleep(tick)
		if !ready {
			// run js in chromedp to open last visible photo
			chromedp.Evaluate(`[...document.querySelectorAll('[data-latest-bg]')].pop().click()`, nil).Do(ctx)
			time.Sleep(tick)
		}
		if err := chromedp.Location(&location).Do(ctx); err != nil {
			return err
		}
		if !ready {
			if location != "https://photos.google.com/" {
				ready = true
				log.Info().Msgf("Nav to the end sequence is started because location is %v", location)
			}
			continue
		}

		if location == prevLocation {
			break
		}
		prevLocation = location
	}
	return nil
}

// doRun runs *runFlag as a command on the given filePath.
func doRun(filePath string) error {
	if *runFlag == "" {
		return nil
	}
	log.Debug().Msgf("Running %v on %v", *runFlag, filePath)
	cmd := exec.Command(*runFlag, filePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// navLeft navigates to the next item to the left
func navWithAction(ctx context.Context, action chromedp.Action) error {
	cl := GetContextLocks(ctx)
	st := time.Now()
	cl.muNavWaiting.Lock()
	cl.listenEvents = true
	cl.muNavWaiting.Unlock()
	action.Do(ctx)
	cl.muNavWaiting.Lock()
	cl.navWaiting = true
	cl.muNavWaiting.Unlock()
	t := time.NewTimer(time.Minute)
	select {
	case <-cl.navDone:
		if !t.Stop() {
			<-t.C
		}
	case <-t.C:
		return errors.New("timeout waiting for navigation")
	}
	cl.muNavWaiting.Lock()
	cl.navWaiting = false
	cl.muNavWaiting.Unlock()
	log.Debug().Msgf("navigation took %dms", time.Since(st).Milliseconds())
	return nil
}

// navLeft navigates to the next item to the left
func navLeft(ctx context.Context) error {
	log.Debug().Msg("Navigating left")
	return navWithAction(ctx, chromedp.KeyEvent(kb.ArrowLeft))
}

// markDone saves location in the dldir/{*lastDoneFlag} file, to indicate it is the
// most recent item downloaded
func markDone(dldir, location string) error {
	log.Debug().Msgf("Marking %v as done", location)

	oldPath := filepath.Join(dldir, *lastDoneFlag)
	newPath := oldPath + ".bak"
	if err := os.Rename(oldPath, newPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.WriteFile(oldPath, []byte(location), 0600); err != nil {
		// restore from backup
		if err := os.Rename(newPath, oldPath); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		}
		return err
	}
	return nil
}

// requestDownload1 sends the Shift+D event, to start the download of the currently
// viewed item.
func requestDownload1(ctx context.Context) error {
	muTabActivity.Lock()
	defer muTabActivity.Unlock()

	log.Debug().Msg("Requesting download (method 1)")
	if err := pressButton(ctx, "D", input.ModifierShift); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	return nil
}

func pressButton(ctx context.Context, key string, modifier input.Modifier) error {
	keyD, ok := kb.Keys[rune(key[0])]
	if !ok {
		return fmt.Errorf("no %s key", key)
	}

	down := input.DispatchKeyEventParams{
		Key:                   keyD.Key,
		Code:                  keyD.Code,
		NativeVirtualKeyCode:  keyD.Native,
		WindowsVirtualKeyCode: keyD.Windows,
		Type:                  input.KeyDown,
		Modifiers:             modifier,
	}
	if runtime.GOOS == "darwin" {
		down.NativeVirtualKeyCode = 0
	}
	up := down
	up.Type = input.KeyUp

	for _, ev := range []*input.DispatchKeyEventParams{&down, &up} {
		log.Trace().Msgf("Triggering button press event: %v, %v, %v", ev.Key, ev.Type, ev.Modifiers)

		if err := chromedp.Run(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// requestDownload2 clicks the icons to start the download of the currently
// viewed item.
func requestDownload2(ctx context.Context, original bool, hasOriginal *bool) error {
	log.Debug().Str("original", fmt.Sprintf("%v", original)).Msg("Requesting download (method 2)")
	originalSelector := `[aria-label="Download original"]`
	var selector string
	if original {
		selector = originalSelector
	} else {
		selector = `[aria-label="Download - Shift+D"]`
	}

	for i := 0; i < 10; i++ {
		muTabActivity.Lock()
		c := chromedp.FromContext(ctx)
		if err := chromedp.Run(ctx,
			target.ActivateTarget(c.Target.TargetID),
		); err != nil {
			log.Printf("ActivateTarget: %v", err)
		}

		page.BringToFront().Do(ctx)
		if err := chromedp.Run(ctx,
			chromedp.ActionFunc(func(ctx context.Context) error {
				// Wait for more options menu to appear
				var nodesTmp []*cdp.Node
				err := doQueryActionWithTimeout(ctx, chromedp.Nodes(`[aria-label="More options"]`, &nodesTmp, chromedp.ByQuery), 2000*time.Millisecond)
				if err == context.DeadlineExceeded {
					return errors.New("more options button not visible")
				}
				return err
			}),

			// Open more options dialog
			chromedp.Evaluate(`[...document.querySelectorAll('[aria-label="More options"]')].pop().click()`, nil),
			// chromedp.Sleep(50*time.Millisecond),

			// Go to download button
			chromedp.ActionFunc(func(ctx context.Context) error {
				// Wait for download button to appear
				var nodesTmp []*cdp.Node
				if err := doQueryActionWithTimeout(ctx, chromedp.Nodes(selector, &nodesTmp, chromedp.ByQuery), 100*time.Millisecond); err != nil && err != context.DeadlineExceeded {
					return err
				}

				// Check if there is an original version of the image that can also be downloaded
				if hasOriginal != nil {
					var dlOriginalNodes []*cdp.Node
					if err := chromedp.Nodes(originalSelector, &dlOriginalNodes, chromedp.AtLeast(0)).Do(ctx); err != nil {
						return err
					}
					*hasOriginal = len(dlOriginalNodes) > 0
				}
				return nil
			}),

			// Press down arrow until the right menu option is selected
			chromedp.ActionFunc(func(ctx context.Context) error {
				var nodes []*cdp.Node
				if err := chromedp.Nodes(selector, &nodes, chromedp.ByQuery, chromedp.AtLeast(0)).Do(ctx); err != nil {
					return err
				}
				if len(nodes) == 0 {
					log.Warn().Msgf("Download button not found for selector %s", selector)
					return errNoDownloadButton
				} else {
					n := 0
					for {
						n++
						if n > 30 {
							return errors.New("was not able to select the download button")
						}
						var style []*css.ComputedStyleProperty
						if err := chromedp.ComputedStyle(selector, &style).Do(ctx); err != nil {
							return err
						}

						for _, s := range style {
							if s.Name == "background-color" && !strings.Contains(s.Value, "0, 0, 0") {
								return nil
							}
						}
						chromedp.KeyEventNode(nodes[0], kb.ArrowDown).Do(ctx)
					}
				}
			}),

			// Activate the selected action and wait a bit before continuing
			chromedp.KeyEvent(kb.Enter),
		); err != nil {
			muTabActivity.Unlock()
			if err == errNoDownloadButton {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			return err
		}

		muTabActivity.Unlock()
		break
	}

	return nil
}

// getPhotoData gets the date from the currently viewed item.
// First we open the info panel by clicking on the "i" icon (aria-label="Open info")
// if it is not already open. Then we read the date from the
// aria-label="Date taken: ?????" field.
func (s *Session) getPhotoData(ctx context.Context) (PhotoData, error) {
	muTabActivity.Lock()
	defer muTabActivity.Unlock()

	var filename string
	var filesize int64 = 0
	var dateStr string
	var timeStr string
	var tzStr string
	timeout1 := time.NewTimer(10 * time.Second)
	timeout2 := time.NewTimer(40 * time.Second)
	log.Debug().Msg("Extracting photo date text and original file name")

	var n = 0
	for {
		if err := page.BringToFront().Do(ctx); err != nil {
			return PhotoData{}, err
		}
		c := chromedp.FromContext(ctx)
		if err := chromedp.Run(ctx,
			target.ActivateTarget(c.Target.TargetID),
		); err != nil {
			log.Printf("ActivateTarget: %v", err)
		}

		n++
		var filesizeStr string

		if err := chromedp.Run(ctx,
			chromedp.Evaluate(`[...document.querySelectorAll('[aria-label^="Filename:"]')].filter(x => x.checkVisibility()).map(x => x.ariaLabel)[0] || ''`, &filename),
			chromedp.Evaluate(`[...document.querySelectorAll('[aria-label^="File size:"]')].filter(x => x.checkVisibility()).map(x => x.ariaLabel)[0] || ''`, &filesizeStr),
			chromedp.Evaluate(`[...document.querySelectorAll('[aria-label^="Date taken:"]')].filter(x => x.checkVisibility()).map(x => x.ariaLabel)[0] || ''`, &dateStr),
			chromedp.Evaluate(`[...document.querySelectorAll('[aria-label^="Date taken:"] + div [aria-label^="Time taken:"]')].filter(x => x.checkVisibility()).map(x => x.ariaLabel)[0] || ''`, &timeStr),
			chromedp.Evaluate(`[...document.querySelectorAll('[aria-label^="Date taken:"] + div [aria-label^="GMT"]')].filter(x => x.checkVisibility()).map(x => x.ariaLabel)[0] || ''`, &tzStr),
		); err != nil {
			return PhotoData{}, err
		}

		if len(filename) > 0 && len(dateStr) > 0 && len(timeStr) > 0 {
			filename = strings.TrimPrefix(filename, "Filename: ")
			dateStr = strings.TrimPrefix(dateStr, "Date taken: ")
			timeStr = strings.TrimPrefix(timeStr, "Time taken: ")
			filesizeStr = strings.Replace(strings.TrimPrefix(filesizeStr, "File size: "), ",", "", -1)
			log.Trace().Msgf("Parsing date: %v and time: %v", dateStr, timeStr)
			log.Trace().Msgf("Parsing filename: %v", filename)
			log.Trace().Msgf("Parsing file size: %v", filesizeStr)

			// Parse file size
			if len(filesizeStr) > 0 {
				var unitFactor int64 = 1
				if s := strings.TrimSuffix(filesizeStr, " B"); s != filesizeStr {
					filesizeStr = s
				} else if s := strings.TrimSuffix(filesizeStr, " KB"); s != filesizeStr {
					unitFactor = 1000
					filesizeStr = s
				} else if s := strings.TrimSuffix(filesizeStr, " MB"); s != filesizeStr {
					unitFactor = 1000 * 1000
					filesizeStr = s
				} else if s := strings.TrimSuffix(filesizeStr, " GB"); s != filesizeStr {
					unitFactor = 1000 * 1000 * 1000
					filesizeStr = s
				}
				filesizeFloat, err := strconv.ParseFloat(strings.TrimSpace(filesizeStr), 64)
				if err != nil {
					return PhotoData{}, err
				}
				filesize = int64(filesizeFloat * float64(unitFactor))
				log.Trace().Msgf("Parsed file size: %v bytes", filesize)
			}

			// Handle dates from current year (UI doesn't show current year so we add it)
			if m, err := regexp.MatchString(`\d{4}$`, dateStr); err == nil && !m {
				dateStr += fmt.Sprintf(", %d", time.Now().Year())
			}

			// Handle special days like "Yesterday" and "Today"
			timeStr = strings.Replace(timeStr, "Yesterday", time.Now().AddDate(0, 0, -1).Format("Mon"), -1)
			timeStr = strings.Replace(timeStr, "Today", time.Now().Format("Mon"), -1)

			// If timezone is not visible, use current timezone (parse provided date to account for DST)
			if len(tzStr) == 0 {
				t, err := time.Parse("Jan 2, 2006", dateStr)
				if err != nil {
					t = time.Now()
				}
				_, offset := t.Zone()
				tzStr = fmt.Sprintf("%+03d%02d", offset/3600, (offset%3600)/60)
			}
			break
		} else {
			log.Trace().Msgf("Incomplete data - Date: %v, Time: %v, Timezone: %v, File name: %v, File size: %v", dateStr, timeStr, tzStr, filename, filesizeStr)

			// Click on info button
			log.Debug().Msg("Date not visible, clicking on i button")
			if err := chromedp.Evaluate(`document.querySelector('[aria-label="Open info"]')?.click()`, nil).Do(ctx); err != nil {
				return PhotoData{}, err
			}

			select {
			case <-timeout1.C:
				if err := navWithAction(ctx, chromedp.Reload()); err != nil {
					return PhotoData{}, err
				}
			case <-timeout2.C:
				return PhotoData{}, fmt.Errorf("timeout waiting for photo info")
			case <-time.After(time.Duration(150+n*12) * time.Millisecond):
			}
		}
	}

	var datetimeStr = strings.Map(func(r rune) rune {
		if r >= 32 && r <= 126 {
			return r
		}
		return -1
	}, dateStr+" "+timeStr) + " " + strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '+' || r == '-' {
			return r
		}
		return -1
	}, tzStr)
	date, err := time.Parse("Jan 2, 2006 Mon, 3:04PM Z0700", datetimeStr)
	if err != nil {
		return PhotoData{}, err
	}

	log.Debug().Msgf("Found date: %v and original filename: %v and file size %d", date, filename, filesize)

	return PhotoData{date, filename, filesize}, nil
}

var dlLock sync.Mutex = sync.Mutex{}

// download starts the download of the currently viewed item, and on successful
// completion saves its location as the most recent item downloaded. It returns
// with an error if the download stops making any progress for more than a minute.
func (s *Session) download(ctx context.Context, location string, dlOriginal bool, hasOriginal *bool, nextDl chan DownloadChannels) (NewDownload, chan bool, error) {
	cl := GetContextLocks(ctx)

	dlLock.Lock()
	defer dlLock.Unlock()

	if len(nextDl) != 0 {
		return NewDownload{}, nil, errors.New("unexpected: nextDl channel is not empty")
	}

	dlStarted := make(chan NewDownload, 1)
	dlProgress := make(chan bool, 1)
	nextDl <- DownloadChannels{dlStarted, dlProgress}

	if err := requestDownload2(ctx, dlOriginal, hasOriginal); err != nil {
		return NewDownload{}, nil, err
	}

	timeout1 := time.NewTimer(30 * time.Second)
	timeout2 := time.NewTimer(60 * time.Second)

	for {
		// Checking for gphotos warning that this video can't be downloaded (no known solution)
		// This check only works for requestDownload2 method (not requestDownload1)
		var nodes []*cdp.Node
		if err := chromedp.Nodes(`[aria-label="Video is still processing & can be downloaded later"] button`, &nodes, chromedp.ByQuery, chromedp.AtLeast(0)).Do(ctx); err != nil {
			return NewDownload{}, nil, err
		}
		isStillProcessing := len(nodes) > 0
		if isStillProcessing {
			// Click the button to close the warning, otherwise it will block navigating to the next photo
			cl.muKbEvents.Lock()
			err := chromedp.MouseClickNode(nodes[0]).Do(ctx)
			cl.muKbEvents.Unlock()
			if err != nil {
				return NewDownload{}, nil, err
			}
		} else {
			// This check only works for requestDownload1 method (not requestDownload2)
			if err := chromedp.Evaluate("document.body.textContent.indexOf('Video is still processing &amp; can be downloaded later') != -1", &isStillProcessing).Do(ctx); err != nil {
				return NewDownload{}, nil, err
			}
			if isStillProcessing {
				time.Sleep(5 * time.Second) // Wait for error message to disappear before continuing, otherwise we will also skip next files
			}
			if !isStillProcessing {
				// Sometimes Google returns a different error, check for that too
				if err := chromedp.Evaluate("document.body.textContent.indexOf('No webpage was found for the web address:') != -1", &isStillProcessing).Do(ctx); err != nil {
					return NewDownload{}, nil, err
				}
				if isStillProcessing {
					log.Info().Msgf("This is an error page, we will navigate back to the photo to be able to continue: %s", location)
					if err := navWithAction(ctx, chromedp.NavigateBack()); err != nil {
						return NewDownload{}, nil, err
					}
					time.Sleep(400 * time.Millisecond)
				}
			}
		}
		if isStillProcessing {
			log.Warn().Msg("Received 'Video is still processing' error")
			select {
			case <-nextDl: // clear nextDl
			default:
			}
			return NewDownload{}, nil, errStillProcessing
		}

		select {
		case <-timeout1.C:
			if err := requestDownload1(ctx); err != nil {
				return NewDownload{}, nil, err
			}
		case <-timeout2.C:
			return NewDownload{}, nil, fmt.Errorf("timeout waiting for download to start for %v", location)
		case newDl := <-dlStarted:
			log.Trace().Msgf("dlStarted: %v", newDl)
			return newDl, dlProgress, nil
		default:
			time.Sleep(25 * time.Millisecond)
		}
	}
}

func imageIdFromUrl(location string) (string, error) {
	// Parse the URL
	u, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("invalid URL %v: %w", location, err)
	}

	// Split the path into segments
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	// Look for "photo" segment and ensure there's a following segment
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "photo" {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("could not find /photo/{imageId} pattern in URL: %v", location)
}

// makeOutDir creates a directory in s.dlDir named of the item ID found in
// location
func (s *Session) makeOutDir(location string) (string, error) {
	imageId, err := imageIdFromUrl(location)
	if err != nil {
		return "", err
	}

	newDir := filepath.Join(s.dlDir, imageId)
	if err := os.MkdirAll(newDir, 0700); err != nil {
		return "", err
	}
	return newDir, nil
}

// dlAndProcess creates a directory in s.dlDir named of the item ID found in
// location. It then moves dlFile in that directory. It returns the new path
// of the moved file.
func (s *Session) dlAndProcess(ctx context.Context, location string) chan error {
	outerErrChan := make(chan error, 1)
	jobs := [](chan error){}

	go func() {
		ctx, cancel := chromedp.NewContext(ctx)
		defer cancel()
		ctx = SetContextLocks(ctx)
		listenNavEvents(ctx)

		nextDl := s.nextDl

		resp, err := chromedp.RunResponse(ctx, chromedp.Navigate(location))
		if err != nil {
			outerErrChan <- err
			return
		}
		if resp.Status == http.StatusOK {
			chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
		} else {
			outerErrChan <- fmt.Errorf("unexpected response: %v", resp.Status)
			return
		}
		time.Sleep(50 * time.Millisecond)

		photoDataChan := make(chan PhotoData, 2)
		go func() {
			data, err := s.getPhotoData(ctx)
			if err != nil {
				outerErrChan <- err
			} else {
				// We may need two of these:
				photoDataChan <- data
				photoDataChan <- data
			}
		}()

		dlHandler := func(dl NewDownload, dlProgress chan bool, errChan chan error, isOriginal bool) {
			dlTimeout := time.NewTimer(time.Minute)
		progressLoop:
			for {
				select {
				case p := <-dlProgress:
					if p {
						break progressLoop
					} else {
						dlTimeout.Reset(time.Minute)
					}
				case <-dlTimeout.C:
					errChan <- fmt.Errorf("timeout waiting for download to complete for %v", location)
					return
				}
			}

			data := <-photoDataChan

			outDir, err := s.makeOutDir(location)
			if err != nil {
				errChan <- err
				return
			}

			var filePaths []string
			if strings.HasSuffix(dl.suggestedFilename, ".zip") {
				var err error
				filePaths, err = s.handleZip(filepath.Join(s.dlDirTmp, dl.GUID), outDir)
				if err != nil {
					errChan <- err
					return
				}
			} else {
				var filename string
				if dl.suggestedFilename != "download" && dl.suggestedFilename != "" {
					filename = dl.suggestedFilename
				} else {
					filename = data.filename
				}

				if isOriginal {
					// to ensure the filename is not the same as the other download, change e.g. image_1.jpg to image_1_original.jpg
					ext := filepath.Ext(filename)
					filename = strings.TrimSuffix(filename, ext) + originalSuffix + ext
				}

				newFile := filepath.Join(outDir, filename)
				log.Debug().Msgf("Moving %v to %v", dl.GUID, newFile)
				if err := os.Rename(filepath.Join(s.dlDirTmp, dl.GUID), newFile); err != nil {
					errChan <- err
					return
				}
				filePaths = []string{newFile}
			}

			if err := s.doFileDateUpdate(ctx, data.date, filePaths); err != nil {
				errChan <- err
				return
			}

			for _, f := range filePaths {
				if err := doRun(f); err != nil {
					errChan <- err
					return
				}
			}

			errChan <- nil
		}

		errChan1 := make(chan error, 1)
		jobs = append(jobs, errChan1)
		hasOriginalChan := make(chan bool, 1)

		go func() {
			hasOriginal := false
			dl, dlProgress, err := s.download(ctx, location, false, &hasOriginal, nextDl)
			if err != nil {
				dlScreenshot(ctx, filepath.Join(s.dlDir, "error"))
				outerErrChan <- err
			} else {
				hasOriginalChan <- hasOriginal
				go dlHandler(dl, dlProgress, errChan1, false)
			}
		}()

		if <-hasOriginalChan {
			errChan2 := make(chan error, 1)
			jobs = append(jobs, errChan2)

			go func() {
				dl, dlProgress, err := s.download(ctx, location, false, nil, nextDl)
				if err != nil {
					dlScreenshot(ctx, filepath.Join(s.dlDir, "error"))
					outerErrChan <- err
				} else {
					go dlHandler(dl, dlProgress, errChan2, false)
				}
			}()
		}

		go func() {
			for i, job := range jobs {
				select {
				case err := <-job:
					if err != nil {
						outerErrChan <- err
						return
					} else {
						jobs = append(jobs[:i], jobs[i+1:]...)
					}
				default:
					time.Sleep(10 * time.Millisecond)
				}
				if len(jobs) == 0 {
					break
				}
			}
			outerErrChan <- nil
		}()
	}()

	return outerErrChan
}

// handleZip handles the case where the currently item is a zip file. It extracts
// each file in the zip file to the same folder, and then deletes the zip file.
func (s *Session) handleZip(zipfile, outFolder string) ([]string, error) {
	st := time.Now()
	log.Debug().Msgf("Unzipping %v in %v", zipfile, outFolder)
	// unzip the file
	files, err := zip.Unzip(zipfile, outFolder)
	if err != nil {
		return []string{""}, err
	}

	// delete the zip file
	if err := os.Remove(zipfile); err != nil {
		return []string{""}, err
	}

	log.Debug().Msgf("Unzipped %v in %v", zipfile, time.Since(st))
	return files, nil
}

var muTabActivity sync.Mutex = sync.Mutex{}

type ContextLocks = struct {
	muNavWaiting             sync.RWMutex
	muKbEvents               sync.Mutex
	listenEvents, navWaiting bool
	navDone                  chan bool
}
type ContextLocksPointer = *ContextLocks

// contextKey is a custom type for context keys to avoid collisions
type contextKey struct {
	name string
}

// Define the key for context locks
var contextLocksKey = &contextKey{name: "contextLocks"}

func GetContextLocks(ctx context.Context) ContextLocksPointer {
	return ctx.Value(contextLocksKey).(ContextLocksPointer)
}

func SetContextLocks(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextLocksKey, &ContextLocks{
		muNavWaiting: sync.RWMutex{},
		muKbEvents:   sync.Mutex{},
		listenEvents: false,
		navWaiting:   false,
		navDone:      make(chan bool, 1),
	})
}

func listenNavEvents(ctx context.Context) {
	cl := GetContextLocks(ctx)
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		cl.muNavWaiting.RLock()
		listen := cl.listenEvents
		cl.muNavWaiting.RUnlock()
		if !listen {
			return
		}
		switch ev.(type) {
		case *page.EventNavigatedWithinDocument:
			go func() {
				for {
					cl.muNavWaiting.RLock()
					waiting := cl.navWaiting
					cl.muNavWaiting.RUnlock()
					if waiting {
						cl.navDone <- true
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
			}()
		}
	})
}

func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

// This function can be used instead of NavN to resync the list of photos
// Use [...document.querySelectorAll('a[href^="./photo/"]')] to find all visible photos
// Check that each one is already downloaded. Optionally check/update date from the element
// attr, e.g. aria-label="Photo - Landscape - Feb 12, 2025, 6:34:39 PM"
// Then do .pop().focus() on the last a element found to scroll to it and make more photos visible
// Then repeat until we get to the end
// If any photos are missing we can asynchronously create a new chromedp context, then in that
// context navigate to that photo and call dlAndProcess
func (s *Session) resync() func(context.Context) error {
	return func(ctx context.Context) error {
		listenNavEvents(ctx)

		asyncJobs := []Job{}
		photoIds := []string{}
		lastNode := &cdp.Node{}
		n := 0
		dlCnt := 0
		retries := 0
		for {
			// find all currently visible photos
			var nodes []*cdp.Node
			if err := chromedp.Run(ctx,
				chromedp.Nodes(`a[href^="./photo/"]`, &nodes, chromedp.ByQueryAll),
			); err != nil {
				return err
			}

			if len(nodes) == 0 {
				log.Info().Msg("No photos to resync")
				break
			}

			sliderPos := 0.0
			var sliderNodes []*cdp.Node
			if err := chromedp.Run(ctx,
				chromedp.Nodes(`div[role="slider"][aria-valuemax="1"][aria-valuetext]`, &sliderNodes, chromedp.ByQuery, chromedp.AtLeast(0)),
			); err != nil {
				return err
			}
			if len(sliderNodes) > 0 {
				posStr, exists := sliderNodes[0].Attribute("aria-valuenow")
				if exists {
					pos, err := strconv.ParseFloat(posStr, 64)
					if err == nil {
						sliderPos = pos
					}
				}
			}
			if retries == 0 {
				log.Debug().Msgf("Slider position: %v", sliderPos)
			}

			// scroll to the last one by focusing the last node
			if lastNode == nodes[len(nodes)-1] {
				if retries > 500 || (retries > 40 && sliderPos > 0.98) || (retries > 4 && sliderPos > 0.995) {
					break
				}
				time.Sleep(250 * time.Millisecond)
				retries++
				continue
			} else {
				retries = 0
			}
			for i, node := range nodes {
				if node == lastNode {
					nodes = nodes[i+1:]
					break
				}
			}
			lastNode = nodes[len(nodes)-1]
			if err := dom.Focus().WithNodeID(lastNode.NodeID).Do(ctx); err != nil {
				return err
			}

			n = n + len(nodes)

			// check that each one is already downloaded
			for _, node := range nodes {
				href := node.AttributeValue("href")
				imageId, err := imageIdFromUrl(href)
				if err != nil {
					return err
				}
				photoIds = append(photoIds, imageId)

				entries, err := os.ReadDir(filepath.Join(s.dlDir, imageId))
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}

				if len(entries) == 0 {
					log.Info().Msgf("Photo %v is missing. Downloading it.", imageId)
					// asynchronously create a new chromedp context, then in that
					// context navigate to that photo and call dlAndProcess
					location := "https://photos.google.com/photo/" + imageId
					asyncJobs = append(asyncJobs, Job{location, s.dlAndProcess(ctx, location)})
					if err := s.processJobs(&asyncJobs, 4, false); err != nil {
						return err
					}
					dlCnt++
				}
			}

			log.Info().Msgf("resynced %v items, downloaded %v new items", n, dlCnt)
		}

		// Check if there are folders in the dl dir that were not seen in gphotos
		entries, err := os.ReadDir(s.dlDir)
		if err != nil {
			return err
		}

		deletedCnt := 0
		for _, entry := range entries {
			if entry.IsDir() && entry.Name() != "tmp" {
				// Check if the folder name is in the list of photo IDs
				if !contains(photoIds, entry.Name()) {
					deletedCnt++
					// log.Info().Msgf("Found local photo %v that does not exist in gphotos. It may have been deleted", entry.Name())
				}
			}
		}
		if deletedCnt > 0 {
			log.Info().Msgf("Folders found for %d local photos that don't exist on google photos", deletedCnt)
		}

		return s.processJobs(&asyncJobs, 0, false)
	}
}

// navN successively downloads the currently viewed item, and navigates to the
// next item (to the left). It repeats N times or until the last (i.e. the most
// recent) item is reached. Set a negative N to repeat until the end is reached.
func (s *Session) navN(N int) func(context.Context) error {
	return func(ctx context.Context) error {
		n := 0
		if N == 0 {
			return nil
		}

		listenNavEvents(ctx)

		var asyncJobs []Job

		var location string
		if err := chromedp.Location(&location).Do(ctx); err != nil {
			return err
		}

		for {
			n++
			if N > 0 && n > N {
				break
			}

			if err := chromedp.Location(&location).Do(ctx); err != nil {
				return err
			}

			imageId, err := imageIdFromUrl(location)
			if err != nil {
				return err
			}
			log.Trace().Msgf("processing %v", imageId)
			entries, err := os.ReadDir(filepath.Join(s.dlDir, imageId))
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}

			var newJob chan error = nil
			if len(entries) == 0 {
				// Local dir doesn't exist or is empty, continue downloading
				newJob = s.dlAndProcess(ctx, location)
			} else if *fixFlag {
				var files []fs.FileInfo
				for _, v := range entries {
					file, err := v.Info()
					if err != nil {
						return err
					}
					files = append(files, file)
				}

				if err := s.checkFile(ctx, files, imageId); err != nil {
					if err == errRetry {
						continue
					}
					return err
				}
			} else {
				log.Debug().Msgf("Skipping %v, file already exists in download dir", imageId)
			}

			asyncJobs = append(asyncJobs, Job{location, newJob})

			if err := s.processJobs(&asyncJobs, *workersFlag, true); err != nil {
				return err
			}

			var morePhotosAvailable bool
			if err := chromedp.Evaluate(`!![...document.querySelectorAll('[aria-label="View previous photo"]')].slice(-1).map(x => window.getComputedStyle(x).display !== 'none')[0]`, &morePhotosAvailable).Do(ctx); err != nil {
				return fmt.Errorf("error checking for nav left button: %v", err)
			}
			if !morePhotosAvailable {
				log.Debug().Str("location", location).Msg("no left button visible, but trying nav left anyway because sometimes it doesn't become visible immediately")
				oldLocation := location
				navLeft(ctx)
				if err := chromedp.Location(&location).Do(ctx); err != nil {
					return err
				}
				if location == oldLocation {
					log.Info().Msgf("no more photos available, we've reached the end of the timeline at %s", location)
					break
				}
			} else {
				if err := navLeft(ctx); err != nil {
					return fmt.Errorf("error at %v: %v", location, err)
				}
			}

			for {
				var res bool
				if err := chromedp.Evaluate("document.body.textContent.indexOf('Your highlight video will be ready soon') != -1", &res).Do(ctx); err != nil {
					return fmt.Errorf("error checking for video processing: %v", err)
				}
				if res {
					if err := navLeft(ctx); err != nil {
						return fmt.Errorf("error at %v: %v", location, err)
					}
				} else {
					break
				}
			}

			if n%500 == 0 {
				log.Info().Msgf("Started %d jobs, waiting for %d jobs to finish", n, len(asyncJobs))
			}
		}

		return s.processJobs(&asyncJobs, 0, true)
	}
}

func (s *Session) processJobs(jobs *[]Job, maxJobs int, doMarkDone bool) error {
	log.Trace().Msgf("Processing %d jobs", len(*jobs))

	n := 0
	for {
		dlCount := 0
		for i := range *jobs {
			select {
			case err := <-(*jobs)[i].errChan:
				(*jobs)[i].errChan = nil
				if err == errStillProcessing {
					// Old highlight videos are no longer available
					log.Info().Msg("Skipping generated highlight video that Google seems to have lost")
				} else if err != nil {
					return err
				}
			case err := <-s.err:
				return err
			default:
			}

			if (*jobs)[i].errChan != nil && len((*jobs)[i].errChan) == 0 {
				dlCount++
			}
		}

		if doMarkDone {
			// Remove completed jobs from the front of the slice
			for len(*jobs) > 0 && (*jobs)[0].errChan == nil {
				if err := markDone(s.dlDir, (*jobs)[0].location); err != nil {
					return err
				}
				*jobs = (*jobs)[1:]
			}

			if n%100 == 0 {
				log.Info().Msgf("%d downloads in progress, %d downloads waiting to be marked as done", dlCount, len(*jobs)-dlCount)
			}

			if len(*jobs) <= maxJobs {
				break
			}
		} else {
			if n%100 == 0 {
				log.Info().Msgf("%d jobs, %d still in progress", len(*jobs), dlCount)
			}

			if dlCount <= maxJobs {
				break
			}
		}

		// Let's wait for some downloads to finish before starting more
		time.Sleep(100 * time.Millisecond)
		n++
	}
	return nil
}

func (s *Session) checkFile(ctx context.Context, files []fs.FileInfo, imageId string) error {
	data, err := s.getPhotoData(ctx)
	if err != nil {
		return err
	}

	var originalFile fs.FileInfo = nil
	var liveFile fs.FileInfo = nil
	if len(files) == 1 {
		originalFile = files[0]
	} else if len(files) > 1 {
		log.Debug().Msgf("there are two files in this dir, checking for original or live photo: %s", strings.Join(fileNames(files), ", "))
		for _, f := range files {
			if strings.Contains(f.Name(), originalSuffix+".") {
				log.Debug().Msgf("found original: %v", f.Name())
				originalFile = f
			}
		}

		if originalFile == nil {
			for _, f := range files {
				if strings.EqualFold(f.Name(), data.filename) {
					log.Debug().Msgf("found probable live photo: %v", f.Name())
					liveFile = f
				}
			}
		}
	}

	if len(files) == 1 && files[0].Size() == 0 {
		log.Debug().Msgf("Removing empty file %v and retrying download", files[0].Name())
		if err := os.Remove(filepath.Join(s.dlDir, imageId, files[0].Name())); err != nil {
			return err
		}
		return errRetry
	}

	if data.fileSize == 0 {
		log.Debug().Msgf("can't check size because the we could not find an expected file size for file: %v", files[0].Name())
	} else {
		var fileOnDiskSize int64 = 0
		if originalFile != nil {
			fileOnDiskSize = originalFile.Size()
		} else if liveFile != nil {
			for _, f := range files {
				fileOnDiskSize += f.Size()
			}
		}

		if fileOnDiskSize == 0 {
			log.Warn().Msgf("can't compare size of unexpected local files: %s", strings.Join(fileNames(files), ", "))
		} else {
			if math.Abs(1-float64(data.fileSize)/float64(fileOnDiskSize)) > 0.15 {
				// No handling for this case yet, just log it
				log.Warn().Msgf("File size mismatch for %s/%s : %v != %v", imageId, data.filename, data.fileSize, fileOnDiskSize)
			}
		}
	}

	var localFilename, processedLocalFilename string
	if originalFile != nil {
		localFilename = originalFile.Name()
		processedLocalFilename = strings.Replace(originalFile.Name(), originalSuffix+".", ".", 1)
	} else if liveFile != nil {
		localFilename = liveFile.Name()
		processedLocalFilename = liveFile.Name()
	}

	if processedLocalFilename == "" {
		log.Warn().Msgf("can't compare filename of unexpected local files: %s", strings.Join(fileNames(files), ", "))
	} else {
		if !strings.EqualFold(processedLocalFilename, data.filename) {
			// No handling for this case yet, just log it
			log.Warn().Msgf("Filename mismatch for %s : %v != %v", imageId, localFilename, data.filename)

		}
	}

	for _, v := range files {
		if math.Abs(v.ModTime().Sub(data.date).Seconds()) > 1 {
			if *fileDateFlag {
				log.Info().Msgf("Setting file date for %v/%v to %v (was %v)", imageId, v.Name(), data.date, v.ModTime())
				if err := setFileDate(filepath.Join(s.dlDir, imageId, v.Name()), data.date); err != nil {
					return err
				}
			} else {
				log.Warn().Msgf("File date mismatch for %s/%s : %v != %v", imageId, v.Name(), v.ModTime(), data.date)
			}
		}
	}

	return nil
}

// doFileDateUpdate updates the file date of the downloaded files to the photo date
func (s *Session) doFileDateUpdate(_ context.Context, date time.Time, filePaths []string) error {
	if !*fileDateFlag {
		return nil
	}

	log.Debug().Msgf("Setting file date for %v", filePaths)

	for _, f := range filePaths {
		if err := setFileDate(f, date); err != nil {
			return err
		}
		log.Info().Msgf("downloaded %v with date %v", filepath.Base(f), date.Format(time.DateOnly))
	}

	return nil
}

func doQueryActionWithTimeout(ctx context.Context, action chromedp.Action, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := action.Do(ctx); err != nil {
		return err
	}
	return nil
}

// Sets modified date of dlFile to given date
func setFileDate(dlFile string, date time.Time) error {
	if err := os.Chtimes(dlFile, date, date); err != nil {
		return err
	}
	return nil
}

func startDlListener(ctx context.Context, nextDl chan DownloadChannels, globalErrChan chan error) {
	dls := make(map[string]DownloadChannels)
	chromedp.ListenBrowser(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadWillBegin); ok {
			log.Debug().Str("GUID", ev.GUID).Msgf("Download of %s started", ev.SuggestedFilename)
			if ev.SuggestedFilename == "downloads.html" {
				return
			}
			if len(nextDl) == 0 {
				globalErrChan <- fmt.Errorf("unexpected download of %s", ev.SuggestedFilename)
			}
			dls[ev.GUID] = <-nextDl
			dls[ev.GUID].newdl <- NewDownload{ev.GUID, ev.SuggestedFilename}
		}
	})

	chromedp.ListenBrowser(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadProgress); ok {
			log.Trace().Msgf("Download event: %v", ev)
			if ev.State == browser.DownloadProgressStateInProgress {
				log.Trace().Str("GUID", ev.GUID).Msgf("Download progress")
				if len(dls[ev.GUID].progress) == 0 {
					dls[ev.GUID].progress <- false
				}
			}
			if ev.State == browser.DownloadProgressStateCompleted {
				log.Debug().Str("GUID", ev.GUID).Msgf("Download completed")
				go func() {
					time.Sleep(time.Second)
					dls[ev.GUID].progress <- true
					delete(dls, ev.GUID)
				}()
			}
		}
	})
}

func fileNames(files []fs.FileInfo) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name()
	}
	return names
}
