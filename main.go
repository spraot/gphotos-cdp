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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/evilsocket/islazy/zip"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
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
	workersFlag  = flag.Int("workers", runtime.NumCPU(), "number of workers to download files concurrently")
)

var tick = 500 * time.Millisecond

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

	log.Info().Msgf("Running with %d workers", *workersFlag)

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
		log.Fatal().Msg(err.Error())
	}
	defer s.Shutdown()

	log.Info().Msgf("Session Dir: %v", s.profileDir)

	if err := s.cleanDlDir(); err != nil {
		log.Fatal().Msg(err.Error())
	}

	ctx, cancel := s.NewContext()
	defer cancel()

	if err := s.login(ctx); err != nil {
		log.Fatal().Msg(err.Error())
	}

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(s.firstNav),
		chromedp.ActionFunc(s.navN(*nItemsFlag)),
	); err != nil {
		log.Fatal().Msg(err.Error())
	}
	fmt.Println("OK")
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
	// firstItem is the most recent item in the feed. It is determined at the
	// beginning of the run, and is used as the final sentinel.
	firstItem string
}

// getLastDone returns the URL of the most recent item that was downloaded in
// the previous run. If any, it should have been stored in dlDir/.lastdone
func getLastDone(dlDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dlDir, ".lastdone"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
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
	}
	return s, nil
}

func (s *Session) NewContext() (context.Context, context.CancelFunc) {
	// Let's use as a base for allocator options (It implies Headless)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.UserDataDir(s.profileDir),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
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
		if v.Name() == ".lastdone" {
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
	return chromedp.Run(ctx,
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).WithDownloadPath(s.dlDirTmp).WithEventsEnabled(true),
		chromedp.ActionFunc(func(ctx context.Context) error {
			log.Debug().Msg("pre-navigate")
			return nil
		}),
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
					dlScreenshot(ctx, filepath.Join(s.dlDir, "error"))
					return errors.New("authentication not possible in -headless mode, see error.png (at " + location + ")")
				}
				log.Debug().Msgf("Not yet authenticated, at: %v", location)
				time.Sleep(tick)
			}
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			log.Debug().Msg("post-navigate")
			return nil
		}),
	)
}

func dlScreenshot(ctx context.Context, filePath string) chromedp.Tasks {
	var buf []byte

	if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		log.Fatal().Msg(err.Error())
	}
	if err := os.WriteFile(filePath+".png", buf, os.FileMode(0666)); err != nil {
		log.Fatal().Msg(err.Error())
	}

	// Dump the HTML to a file
	var html string
	if err := chromedp.Run(ctx, chromedp.OuterHTML("html", &html, chromedp.ByQuery)); err != nil {
		log.Fatal().Msg(err.Error())
	}
	if err := os.WriteFile(filePath+".html", []byte(html), 0640); err != nil {
		log.Fatal().Msg(err.Error())
	}

	return nil
}

// firstNav does either of:
// 1) if a specific photo URL was specified with *startFlag, it navigates to it
// 2) if the last session marked what was the most recent downloaded photo, it navigates to it
// 3) otherwise it jumps to the end of the timeline (i.e. the oldest photo)
func (s *Session) firstNav(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := s.setFirstItem(ctx); err != nil {
		return err
	}

	if *startFlag != "" {
		// TODO(mpl): use RunResponse
		chromedp.Navigate(*startFlag).Do(ctx)
		chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
		return nil
	}
	if s.lastDone != "" {
		resp, err := chromedp.RunResponse(ctx, chromedp.Navigate(s.lastDone))
		if err != nil {
			return err
		}
		if resp.Status == http.StatusOK {
			chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
			return nil
		}
		lastDoneFile := filepath.Join(s.dlDir, ".lastdone")
		log.Info().Msgf("%s does not seem to exist anymore. Removing %s.", s.lastDone, lastDoneFile)
		s.lastDone = ""
		if err := os.Remove(lastDoneFile); err != nil {
			if os.IsNotExist(err) {
				log.Fatal().Msg("Failed to remove .lastdone file because it was already gone.")
			}
			return err
		}

		// restart from scratch
		resp, err = chromedp.RunResponse(ctx, chromedp.Navigate("https://photos.google.com/"))
		if err != nil {
			return err
		}
		code := resp.Status
		if code != http.StatusOK {
			return fmt.Errorf("unexpected %d code when restarting to https://photos.google.com/", code)
		}
		chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
	}

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
	for {
		chromedp.KeyEvent(kb.ArrowRight).Do(ctx)
		time.Sleep(tick)
		attributes := make(map[string]string)
		if err := chromedp.Run(ctx,
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

		s.firstItem = strings.TrimPrefix(photoHref, "./photo/")
		break
	}
	log.Debug().Msgf("Page loaded, most recent item in the feed is: %s", s.firstItem)
	return nil
}

// navToEnd scrolls down to the end of the page, i.e. to the oldest items.
func (s *Session) navToEnd(ctx context.Context) error {
	// try jumping to the end of the page. detect we are there and have stopped
	// moving when two consecutive screenshots are identical.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var previousScr, scr []byte
	for {
		chromedp.KeyEvent(kb.PageDown).Do(ctx)
		chromedp.KeyEvent(kb.End).Do(ctx)
		time.Sleep(tick * 10)
		chromedp.CaptureScreenshot(&scr).Do(ctx)
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
func navLeft(ctx context.Context) error {
	st := time.Now()
	muNavWaiting.Lock()
	listenEvents = true
	muNavWaiting.Unlock()
	chromedp.KeyEvent(kb.ArrowLeft).Do(ctx)
	muNavWaiting.Lock()
	navWaiting = true
	muNavWaiting.Unlock()
	t := time.NewTimer(time.Minute)
	select {
	case <-navDone:
		if !t.Stop() {
			<-t.C
		}
	case <-t.C:
		return errors.New("timeout waiting for left navigation")
	}
	muNavWaiting.Lock()
	navWaiting = false
	muNavWaiting.Unlock()
	log.Debug().Msgf("navLeft took %dms", time.Since(st).Milliseconds())
	return nil
}

// markDone saves location in the dldir/.lastdone file, to indicate it is the
// most recent item downloaded
func markDone(dldir, location string) error {
	log.Debug().Msgf("Marking %v as done", location)

	oldPath := filepath.Join(dldir, ".lastdone")
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

// startDownload sends the Shift+D event, to start the download of the currently
// viewed item.
func startDownload(ctx context.Context) error {
	keyD, ok := kb.Keys['D']
	if !ok {
		return errors.New("no D key")
	}

	down := input.DispatchKeyEventParams{
		Key:                   keyD.Key,
		Code:                  keyD.Code,
		NativeVirtualKeyCode:  keyD.Native,
		WindowsVirtualKeyCode: keyD.Windows,
		Type:                  input.KeyDown,
		Modifiers:             input.ModifierShift,
	}
	if runtime.GOOS == "darwin" {
		down.NativeVirtualKeyCode = 0
	}
	up := down
	up.Type = input.KeyUp

	for _, ev := range []*input.DispatchKeyEventParams{&down, &up} {
		log.Trace().Msgf("Event: %+v", *ev)

		if err := ev.Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

// getPhotoData gets the date and file name from the currently viewed item.
// First we open the info panel by clicking on the "i" icon (aria-label="Open info")
// if it is not already open. Then we read the date from the
// aria-label="Date taken: ?????" field.
func (s *Session) getPhotoData(ctx context.Context, imageId string) (time.Time, string, error) {
	var filename string
	var dateStr string
	var timeStr string
	var tzStr string
	timeout := time.NewTimer(30 * time.Second)
	log.Debug().Str("imageId", imageId).Msg("Extracting photo date text and original file name")

	// check if element [aria-label^="Date taken:"] is visible, if not click i button
	var n = 0
	for {
		n++
		var filenameNodes []*cdp.Node
		var dateNodes []*cdp.Node
		var timeNodes []*cdp.Node
		var tzNodes []*cdp.Node

		if err := chromedp.Run(ctx,
			chromedp.Nodes(`[aria-label^="Filename:"]`, &filenameNodes, chromedp.ByQuery, chromedp.AtLeast(0)),
			chromedp.Nodes(`[aria-label^="Date taken:"]`, &dateNodes, chromedp.ByQuery, chromedp.AtLeast(0)),
			chromedp.Nodes(`[aria-label^="Date taken:"] + div [aria-label^="Time taken:`, &timeNodes, chromedp.ByQuery, chromedp.AtLeast(0)),
			chromedp.Nodes(`[aria-label^="Date taken:"] + div [aria-label^="GMT"]`, &tzNodes, chromedp.ByQuery, chromedp.AtLeast(0)),
		); err != nil {
			return time.Time{}, "", err
		}

		if len(dateNodes) > 0 && len(timeNodes) > 0 && len(filenameNodes) > 0 {
			filename = strings.TrimPrefix(filenameNodes[0].AttributeValue("aria-label"), "Filename: ")
			dateStr = strings.TrimPrefix(dateNodes[0].AttributeValue("aria-label"), "Date taken: ")
			timeStr = strings.TrimPrefix(timeNodes[0].AttributeValue("aria-label"), "Time taken: ")
			if len(tzNodes) > 0 {
				tzStr = tzNodes[0].AttributeValue("aria-label")
			} else {
				t := time.Now()
				_, offset := t.Zone()
				tzStr = fmt.Sprintf("%+03d%02d", offset/3600, (offset%3600)/60)
			}
			break
		}

		log.Info().Str("imageId", imageId).Msg("Date and filename not visible, clicking on i button")
		if n%6 == 0 {
			log.Warn().Str("imageId", imageId).Msg("Date and filename not visible after 3 tries, attempting to resolve by refreshing the page")
			if err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
				return time.Time{}, "", err
			}
		} else {
			if err := chromedp.Run(ctx,
				chromedp.Click(`[aria-label="Open info"]`, chromedp.ByQuery, chromedp.AtLeast(0)),
			); err != nil {
				return time.Time{}, "", err
			}
		}
		select {
		case <-timeout.C:
			dlScreenshot(ctx, filepath.Join(s.dlDir, "error"))
			return time.Time{}, "", fmt.Errorf("timeout waiting for date to appear for %v (see error.png)", imageId)
		case <-time.After(time.Duration(n) * tick):
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
		return time.Time{}, "", err
	}
	log.Debug().Str("imageId", imageId).Msgf("Found date: %v and original filename: %v ", date, filename)
	return date, filename, nil
}

func imageIdFromUrl(location string) (string, error) {
	parts := strings.Split(location, "/")
	if len(parts) < 5 {
		return "", fmt.Errorf("not enough slash separated parts in location %v: %d", location, len(parts))
	}
	return parts[4], nil
}

// makeOutDir creates a directory in s.dlDir named of the item ID found in
// location
func (s *Session) makeOutDir(_ context.Context, job *DownloadJob) (string, error) {
	newDir := filepath.Join(s.dlDir, job.imageId)
	if err := os.MkdirAll(newDir, 0700); err != nil {
		return "", err
	}
	return newDir, nil
}

func (s *Session) dlAndMove(ctx context.Context, job *DownloadJob) error {
	<-job.downloadDone
	log.Debug().Msgf("Download took %dms", time.Since(job.st).Milliseconds())

	outDir, err := s.makeOutDir(ctx, job)
	if err != nil {
		return err
	}

	var filename string
	if job.suggestedFilename != "download" && job.suggestedFilename != "" {
		filename = job.suggestedFilename
	} else {
		filename = job.originalFilename
	}

	if strings.HasSuffix(filename, ".zip") {
		filepaths, err := s.handleZip(ctx, filepath.Join(s.dlDirTmp, job.dlFile), outDir)
		if err != nil {
			return err
		}
		job.storedFiles = append(job.storedFiles, filepaths...)
	} else {
		newFile := filepath.Join(outDir, filename)
		if err := os.Rename(filepath.Join(s.dlDirTmp, job.dlFile), newFile); err != nil {
			return err
		}
		job.storedFiles = append(job.storedFiles, newFile)
	}

	return nil
}

// handleZip handles the case where the currently item is a zip file. It extracts
// each file in the zip file to the same folder, and then deletes the zip file.
func (s *Session) handleZip(_ context.Context, zipfile, outFolder string) ([]string, error) {
	st := time.Now()
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

var (
	muNavWaiting             sync.RWMutex
	listenEvents, navWaiting = false, false
	navDone                  = make(chan bool, 1)
)

func listenNavEvents(ctx context.Context) {
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		muNavWaiting.RLock()
		listen := listenEvents
		muNavWaiting.RUnlock()
		if !listen {
			return
		}
		switch ev.(type) {
		case *page.EventNavigatedWithinDocument:
			go func() {
				for {
					muNavWaiting.RLock()
					waiting := navWaiting
					muNavWaiting.RUnlock()
					if waiting {
						navDone <- true
						break
					}
					time.Sleep(tick)
				}
			}()
		}
	})
}

// navN successively downloads the currently viewed item, and navigates to the
// next item (to the left). It repeats N times or until the last (i.e. the most
// recent) item is reached. Set a negative N to repeat until the end is reached.
func (s *Session) navN(N int) func(context.Context) error {
	return func(ctx context.Context) error {
		if N == 0 {
			return nil
		}

		dm := NewDownloadManager(ctx, s, *workersFlag)
		listenNavEvents(ctx)

		n := 0
		var location, prevLocation string
		activeDownloads := make(map[string]*DownloadJob)

		for {
			if err := chromedp.Location(&location).Do(ctx); err != nil {
				return err
			}
			if location == prevLocation {
				break
			}
			prevLocation = location

			imageId, err := imageIdFromUrl(location)
			if err != nil {
				return err
			}

			log.Debug().Msgf("Processing %v", imageId)

			// Check if we need to download this item
			entries, err := os.ReadDir(filepath.Join(s.dlDir, imageId))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}

			if len(entries) == 0 {
				// Local dir doesn't exist or is empty, start downloading
				readyForNext := make(chan bool, 1)
				job, err := dm.StartDownload(location, imageId, readyForNext)
				if err != nil {
					return err
				}
				activeDownloads[location] = job

				if len(readyForNext) == 0 {
					log.Debug().Msgf("Waiting for download of %v to start", imageId)
				}
				<-readyForNext
				for len(activeDownloads) >= *workersFlag {
					// Wait for some downloads to complete before navigating
					log.Debug().Msgf("There are %v active downloads, waiting for some to complete", len(activeDownloads))
					time.Sleep(tick)
					if err := s.checkJobStates(activeDownloads); err != nil {
						return err
					}
				}
			} else {
				log.Debug().Msgf("Skipping %v, file already exists in download dir", imageId)
				job := &DownloadJob{
					done: make(chan bool, 1),
				}
				job.done <- true
				activeDownloads[location] = job
			}

			if err := s.checkJobStates(activeDownloads); err != nil {
				return err
			}

			n++
			if N > 0 && n >= N {
				break
			}
			if strings.HasSuffix(location, s.firstItem) {
				break
			}

			if err := navLeft(ctx); err != nil {
				return fmt.Errorf("error at %v: %v", location, err)
			}
		}

		// Wait for remaining downloads to complete
		for {
			if err := s.checkJobStates(activeDownloads); err != nil {
				return err
			}
			if len(activeDownloads) == 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		return nil
	}
}

func (s *Session) checkJobStates(activeDownloads map[string]*DownloadJob) error {
	allDone := true
	for loc, job := range activeDownloads {
		select {
		case <-job.done:
			if allDone {
				// Mark as done only if all previous downloads are done
				if err := markDone(s.dlDir, loc); err != nil {
					job.err <- err
					continue
				}
				delete(activeDownloads, loc)
			} else {
				job.done <- true
			}
		case err := <-job.err:
			return fmt.Errorf("download error for %s: %v", loc, err)
		default:
			// Download still in progress
			allDone = false
		}
	}
	return nil
}

// doFileDateUpdate updates the file date of the downloaded files to the photo date
func (s *Session) doFileDateUpdate(ctx context.Context, job *DownloadJob) error {
	if !*fileDateFlag {
		return nil
	}

	storedFilenames := []string{}
	for _, f := range job.storedFiles {
		storedFilenames = append(storedFilenames, filepath.Base(f))
		if err := s.setFileDate(ctx, f, job.timeTaken); err != nil {
			return err
		}
	}

	log.Info().
		Str("id", job.imageId).
		Str("suggestedFilename", job.suggestedFilename).
		Int("processingTime", int(time.Since(job.st).Milliseconds())).
		Msgf("downloaded %v with date %v", strings.Join(storedFilenames, ", "), job.timeTaken.Format(time.DateOnly))

	return nil
}

// Sets modified date of dlFile to given date
func (s *Session) setFileDate(_ context.Context, dlFile string, date time.Time) error {
	if err := os.Chtimes(dlFile, date, date); err != nil {
		return err
	}
	return nil
}
