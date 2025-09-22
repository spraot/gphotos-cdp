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
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evilsocket/islazy/zip"
	"golang.org/x/text/unicode/norm"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"slices"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	nItemsFlag      = flag.Int("n", -1, "number of items to download. If negative, get them all.")
	devFlag         = flag.Bool("dev", false, "dev mode. we reuse the same session dir (/tmp/gphotos-cdp), so we don't have to auth at every run.")
	downloadDirFlag = flag.String("dldir", "", "where to write the downloads. defaults to $HOME/Downloads/gphotos-cdp.")
	profileFlag     = flag.String("profile", "", "like -dev, but with a user-provided profile dir")
	fromFlag        = flag.String("from", "", "earliest date to sync (YYYY-MM-DD)")
	toFlag          = flag.String("to", "", "latest date to sync (YYYY-MM-DD)")
	untilFlag       = flag.String("until", "", "stop syncing at this photo")
	runFlag         = flag.String("run", "", "the program to run on each downloaded item, right after it is dowloaded. It is also the responsibility of that program to remove the downloaded item, if desired.")
	verboseFlag     = flag.Bool("v", false, "be verbose")
	headlessFlag    = flag.Bool("headless", false, "Start chrome browser in headless mode (must use -dev and have already authenticated).")
	jsonLogFlag     = flag.Bool("json", false, "output logs in JSON format")
	logLevelFlag    = flag.String("loglevel", "", "log level: debug, info, warn, error, fatal, panic")
	removedFlag     = flag.Bool("removed", false, "save list of files found locally that appear to be deleted from Google Photos")
	workersFlag     = flag.Int64("workers", 1, "number of concurrent downloads allowed")
	albumIdFlag     = flag.String("album", "", "ID of album to download, has no effect if lastdone file is found or if -start contains full URL")
	albumTypeFlag   = flag.String("albumtype", "album", "type of album to download (as seen in URL), has no effect if lastdone file is found or if -start contains full URL")
	batchSizeFlag   = flag.Int("batchsize", 0, "number of photos to download in one batch")
	execPathFlag    = flag.String("execpath", "", "path to Chrome/Chromium binary to use")
)

const gphotosUrl = "https://photos.google.com"
const tick = 500 * time.Millisecond
const originalSuffix = "_original"

var errStillProcessing = errors.New("video is still processing & can be downloaded later")
var errCouldNotPressDownloadButton = errors.New("could not press download button")
var errPhotoTakenBeforeFromDate = errors.New("photo taken before from date")
var errPhotoTakenAfterToDate = errors.New("photo taken after to date")
var errAlreadyDownloaded = errors.New("photo already downloaded")
var errAbortBatch = errors.New("abort batch")
var errNavigateAborted = errors.New("navigate aborted")
var errUnexpectedDownload = errors.New("unexpected download")
var fromDate time.Time
var toDate time.Time
var loc GPhotosLocale

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
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.TimeOnly})
	}
	if (!*devFlag && *profileFlag == "") && *headlessFlag {
		log.Fatal().Msg("-headless only allowed in dev mode or if -profile dir is set")
	}
	if *devFlag && *profileFlag != "" {
		log.Fatal().Msg("only one of -dev and -profile can be used")
	}
	if *albumIdFlag != "" && (*fromFlag != "" || *toFlag != "") {
		log.Fatal().Msg("-from and -to cannot be used with -album")
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

	if *fromFlag != "" {
		var err error
		fromDate, err = time.Parse(time.DateOnly, *fromFlag)
		if err != nil {
			log.Fatal().Msgf("could not parse -from argument %s, must be YYYY-MM-DD", *fromFlag)
		}
	}
	if *toFlag != "" {
		var err error
		toDate, err = time.Parse(time.DateOnly, *toFlag)
		toDate = toDate.Add(time.Hour * 24)
		if err != nil {
			log.Fatal().Msgf("could not parse -to argument %s, must be YYYY-MM-DD", *toFlag)
		}
	}

	s, err := NewSession()
	if err != nil {
		log.Fatal().Msgf("failed to create session: %v", err)
	}
	defer s.Shutdown()

	log.Info().Msgf("session dir: %v", s.profileDir)

	if err := s.cleanDownloadDir(); err != nil {
		log.Fatal().Msgf("failed to clean download directory %v: %v", s.downloadDir, err)
	}

	ctx, cancel := s.NewWindow()
	defer cancel()

	startupCtx, startupCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer startupCancel()

	if err := s.login(startupCtx); err != nil {
		log.Fatal().Msgf("login failed: %v", err)
	}

	locale, err := s.getLocale(startupCtx)
	if err != nil {
		log.Fatal().Msgf("failed to get locale: %v", err)
	}

	initLocales()
	_loc, exists := locales[locale]
	if !exists {
		log.Warn().Msgf("your Google account is using unsupported locale %s, this is likely to cause issues. Please change account language to English (en) or another supported locale", locale)
		loc = locales["en"]
	} else {
		log.Info().Msgf("using locale %s", locale)
		loc = _loc
	}

	if err := chromedp.Run(startupCtx,
		chromedp.ActionFunc(s.firstNav),
	); err != nil {
		log.Fatal().Msgf("failed to run first nav: %v", err)
	}
	startupCancel()

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(s.resync),
		chromedp.ActionFunc(s.checkForRemovedFiles),
	); err != nil {
		log.Fatal().Msgf("failure during sync: %v", err)
	}

	log.Info().Msg("Done")
}

type PhotoData struct {
	date     time.Time
	filename string
}

type Job struct {
	imageIds []string
}

type NewDownload struct {
	GUID              string
	suggestedFilename string
	targetId          string
	progressChan      chan bool
}

type Session struct {
	parentContext    context.Context
	chromeExecCancel context.CancelFunc
	downloadDir      string // dir where the photos get stored
	downloadDirTmp   string // dir where the photos get stored temporarily
	profileDir       string // user data session dir. automatically created on chrome startup.
	startNodeParent  *cdp.Node
	globalErrChan    chan error
	userPath         string
	albumPath        string
	existingItems    sync.Map
	foundItems       sync.Map
	downloadedItems  sync.Map
	newDownloadChan  chan NewDownload
	skippedCount     atomic.Uint64
}

func NewSession() (*Session, error) {
	albumPath := ""
	userPath := ""
	if *albumIdFlag != "" {
		i := strings.LastIndex(*albumIdFlag, "/")
		if i != -1 {
			if *albumTypeFlag != "album" {
				log.Warn().Msgf("-albumtype argument is ignored because it looks like given album ID already contains a type: %v", *albumIdFlag)
			}
			albumPath = "/" + *albumIdFlag
			*albumIdFlag = albumPath[i+1:]
		} else {
			albumPath = "/" + *albumTypeFlag + "/" + *albumIdFlag
		}
	}
	if strings.HasPrefix(albumPath, "/u/") {
		a := strings.Index(albumPath[3:], "/")
		if a == -1 {
			userPath = albumPath
			albumPath = ""
		} else {
			userPath = albumPath[:a+3]
			albumPath = albumPath[a+3:]
		}
	}
	log.Info().Msgf("syncing files at root dir %s%s%s", gphotosUrl, userPath, albumPath)
	var dir string
	if *devFlag {
		dir = filepath.Join(os.TempDir(), "gphotos-cdp")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	} else if *profileFlag != "" {
		dir = *profileFlag
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
	downloadDir := *downloadDirFlag
	if downloadDir == "" {
		downloadDir = filepath.Join(os.Getenv("HOME"), "Downloads", "gphotos-cdp")
	}
	if err := os.MkdirAll(downloadDir, 0700); err != nil {
		return nil, err
	}

	downloadDirEntries, err := os.ReadDir(downloadDir)
	if err != nil {
		return nil, err
	}

	downloadDirTmp := filepath.Join(downloadDir, "tmp")
	if err := os.MkdirAll(downloadDirTmp, 0700); err != nil {
		return nil, err
	}

	s := &Session{
		profileDir:      dir,
		downloadDir:     downloadDir,
		downloadDirTmp:  downloadDirTmp,
		globalErrChan:   make(chan error, 1),
		userPath:        userPath,
		albumPath:       albumPath,
		newDownloadChan: make(chan NewDownload),
	}

	for _, e := range downloadDirEntries {
		if e.IsDir() && e.Name() != "tmp" {
			s.existingItems.Store(e.Name(), struct{}{})
		}
	}

	return s, nil
}

func (s *Session) NewWindow() (context.Context, context.CancelFunc) {
	log.Info().Msgf("starting Chrome browser")

	// Let's use as a base for allocator options (It implies Headless)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(s.profileDir),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("lang", "en-US,en"),
		chromedp.Flag("accept-lang", "en-US,en"),
		chromedp.Flag("window-size", "1920,1080"),
		chromedp.Flag("enable-logging", true),
		chromedp.Flag("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/111.0.0.0 Safari/537.36"),
	)

	if !*headlessFlag {
		// undo the three opts in chromedp.Headless() which is included in DefaultExecAllocatorOptions
		opts = append(opts, chromedp.Flag("headless", false))
		opts = append(opts, chromedp.Flag("hide-scrollbars", false))
		opts = append(opts, chromedp.Flag("mute-audio", false))
	}

	if *execPathFlag != "" {
		opts = append(opts, chromedp.ExecPath(*execPathFlag))
	}

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	s.chromeExecCancel = cancel

	ctx, cancel = chromedp.NewContext(ctx,
		chromedp.WithLogf(s.cdpLog),
		chromedp.WithDebugf(s.cdpDebug),
		chromedp.WithErrorf(s.cdpError),
	)
	s.parentContext = ctx
	ctx = SetContextData(ctx)

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			c := chromedp.FromContext(ctx)
			if err := browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).WithDownloadPath(s.downloadDirTmp).WithEventsEnabled(true).
				// use the Browser executor so that it does not pass "sessionId" to the command.
				Do(cdp.WithExecutor(ctx, c.Browser)); err != nil {
				return err
			}

			_, product, _, _, _, err := browser.GetVersion().Do(ctx)
			if err != nil {
				return err
			}
			log.Info().Msgf("Browser version: %s", product)
			return nil
		}),
	); err != nil {
		panic(err)
	}

	startDownloadListener(ctx, s.newDownloadChan)

	return ctx, cancel
}

func (s *Session) Shutdown() {
	s.chromeExecCancel()
}

func (s *Session) cdpLog(format string, v ...any) {
	if !strings.Contains(format, "unhandled") && !strings.Contains(format, "event") {
		log.Debug().Msgf(format, v...)
	}
}

func (s *Session) cdpDebug(format string, v ...any) {
	log.Trace().Msgf(format, v...)
}

func (s *Session) cdpError(format string, v ...any) {
	if !strings.Contains(format, "unhandled") && !strings.Contains(format, "event") {
		log.Error().Msgf(format, v...)
	}
}

// cleanDownloadDir removes all files (but not directories) from s.downloadDir
func (s *Session) cleanDownloadDir() error {
	if s.downloadDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.downloadDirTmp)
	if err != nil {
		return err
	}
	for _, v := range entries {
		if v.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(s.downloadDirTmp, v.Name())); err != nil {
			return err
		}
	}
	return nil
}

// login navigates to https://photos.google.com/login and waits for the user to have
// authenticated (or for 2 minutes to have elapsed).
func (s *Session) login(ctx context.Context) error {
	log.Info().Msg("starting authentication...")
	return chromedp.Run(ctx,
		chromedp.Navigate("https://photos.google.com/login"),
		// when we're not authenticated, the URL is actually
		// https://www.google.com/photos/about/ , so we rely on that to detect when we have
		// authenticated.
		chromedp.ActionFunc(func(ctx context.Context) error {
			tick := 2 * time.Second
			timeout := time.Now().Add(2 * time.Minute)
			var location string
			for {
				if err := chromedp.Location(&location).Do(ctx); err != nil {
					return err
				}
				if strings.HasPrefix(location, gphotosUrl) {
					return nil
				}
				if strings.Contains(location, "signinchooser") {
					userIndex := 0
					if s.userPath != "" {
						userIndex, _ = strconv.Atoi(s.userPath[2:])
					}
					if err := chromedp.Evaluate(`document.querySelector('[data-authuser][data-item-index="`+strconv.Itoa(userIndex)+`"]')?.click()`, nil).Do(ctx); err != nil {
						return err
					}
					time.Sleep(tick)
					continue
				}
				if strings.Contains(location, "signin/challenge/dp") {
					log.Info().Msgf("waiting for user to approve login with other device")
					time.Sleep(tick)
					continue
				}
				if strings.Contains(location, "signin/rejected") {
					return errors.New("google rejected automated login")
				}
				if strings.Contains(location, "signin/speedbump/passkeyenrollment") && loc.NotNow != "" {
					// skip passkey enrollment, press "Not now" button
					if err := chromedp.Click(`//button//span[contains(text(), loc.NotNow)]`, chromedp.BySearch).Do(ctx); err != nil {
						return err
					}
					time.Sleep(tick)
					continue
				}
				var nodes []*cdp.Node
				email := os.Getenv("GPHOTOS_EMAIL")
				if email != "" {
					email_node := "#identifierId:not([type=hidden])"
					log.Debug().Msgf("checking for email node: %s", email_node)
					if err := chromedp.Nodes(email_node, &nodes, chromedp.ByQuery, chromedp.AtLeast(0)).Do(ctx); err != nil {
						return err
					}
					if len(nodes) > 0 {
						log.Info().Msgf("logging in with user email: %s", email)
						if err := chromedp.SendKeys(email_node, email+kb.Enter).Do(ctx); err != nil {
							return err
						}
						time.Sleep(tick)
						continue
					}
				}
				password := os.Getenv("GPHOTOS_PASSWORD")
				if password != "" {
					password_node := "input[name=Passwd]"
					log.Debug().Msgf("checking for password node: %s", password_node)
					if err := chromedp.Nodes(password_node, &nodes, chromedp.ByQuery, chromedp.AtLeast(0)).Do(ctx); err != nil {
						return err
					}
					if len(nodes) > 0 {
						log.Info().Msgf("logging in with user password")
						if err := chromedp.SendKeys(password_node, password+kb.Enter).Do(ctx); err != nil {
							return err
						}
						time.Sleep(tick * time.Duration(3))
						continue
					}
				}
				if *headlessFlag {
					captureScreenshot(ctx, filepath.Join(s.downloadDir, "error"))
					return errors.New("authentication not possible in -headless mode, see error.png (URL=" + location + ")")
				}
				if time.Now().After(timeout) {
					return errors.New("timeout waiting for authentication")
				}
				log.Debug().Msgf("not yet authenticated, at: %v", location)
				time.Sleep(tick)
			}
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			log.Info().Msg("successfully authenticated")
			return nil
		}),
	)
}

func (s *Session) getLocale(ctx context.Context) (string, error) {
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
		log.Warn().Err(err).Msg("failed to detect account locale, will assume English (en)")
		return "en", nil
	}

	return locale, nil
}

func captureScreenshot(ctx context.Context, filePath string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var buf []byte

	log.Trace().Msgf("saving screenshot to %v", filePath+".png")
	if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		log.Err(err).Msgf("failed to capture screenshot: %v", err)
	} else if err := os.WriteFile(filePath+".png", buf, os.FileMode(0666)); err != nil {
		log.Err(err).Msgf("failed to write screenshot: %v", err)
	}

	// Dump the HTML to a file
	var html string
	if err := chromedp.Run(ctx, chromedp.OuterHTML("html", &html, chromedp.ByQuery)); err != nil {
		log.Err(err).Msgf("failed to get HTML: %v", err)
	} else if err := os.WriteFile(filePath+".html", []byte(html), 0640); err != nil {
		log.Err(err).Msgf("failed to write HTML: %v", err)
	}
}

// firstNav does either of:
// 1) if a specific photo URL was specified with *startFlag, it navigates to it
// 2) if the last session marked what was the most recent downloaded photo, it navigates to it
// 3) otherwise it jumps to the end of the timeline (i.e. the oldest photo)
func (s *Session) firstNav(ctx context.Context) (err error) {
	if s.userPath+s.albumPath != "" {
		if err := s.navigateWithAction(ctx, log.Logger, chromedp.Navigate(gphotosUrl+s.userPath+s.albumPath), "to start", 20000*time.Millisecond, 5); err != nil {
			return err
		}
		chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
	}

	// This is only used to ensure page is loaded
	if err := s.setFirstItem(ctx); err != nil {
		return err
	}

	if *toFlag != "" {
		t, err := time.Parse("2006-01-02", *toFlag)
		if err != nil {
			log.Err(err).Msgf("error parsing -to argument '%s': %s", *toFlag, err.Error())
			return errors.New("-to argument must be of format 'YYYY-MM-DD'")
		}
		startDate := t

		time.Sleep(500 * time.Millisecond)
		log.Info().Msgf("attempting to scroll to %v", startDate)

		if err := s.navToEnd(ctx); err != nil {
			return err
		}

		// Find class name for date nodes
		dateNodesClassName := ""
		for range 20 {
			chromedp.Evaluate(`
				document.querySelector('`+getAriaLabelSelector(loc.SelectAllPhotosLabel)+`').parentNode.childNodes[1].childNodes[0].childNodes[0].childNodes[0].className
				`, &dateNodesClassName).Do(ctx)
			if dateNodesClassName != "" {
				break
			}
			chromedp.KeyEvent(kb.PageUp).Do(ctx)
			time.Sleep(100 * time.Millisecond)
		}
		if dateNodesClassName == "" {
			return errors.New("failed to find date nodes class name")
		}

		bisectBounds := []float64{0.0, 1.0}
		scrollPos := 0.0
		var foundDateNode, matchedNode *cdp.Node
		for range 100 {
			scrollTarget := (bisectBounds[0] + bisectBounds[1]) / 2
			log.Debug().Msgf("scrolling to %.2f%%", scrollTarget*100)
			for range 20 {
				if err := setScrollPosition(ctx, scrollTarget); err != nil {
					return err
				}
				time.Sleep(100 * time.Millisecond)
				if err := getScrollPosition(ctx, &scrollPos); err != nil {
					return err
				}
				if math.Abs(scrollPos-scrollTarget) < 0.002 {
					break
				}
			}
			log.Trace().Msgf("scroll position: %.4f%%", scrollPos*100)

			var dateNodes []*cdp.Node
			for range 20 {
				if err := chromedp.Nodes(`div.`+dateNodesClassName, &dateNodes, chromedp.ByQueryAll, chromedp.AtLeast(0)).Do(ctx); err != nil {
					return errors.New("failed to get visible date nodes, " + err.Error())
				}
				if len(dateNodes) > 0 {
					break
				}
				chromedp.KeyEvent(kb.PageUp).Do(ctx)
				time.Sleep(500 * time.Millisecond)
			}
			if len(dateNodes) == 0 {
				return errors.New("no date nodes found")
			}

			var closestDateNode *cdp.Node
			var closestDateDiff int
			var knownFirstOccurance bool
			for i, n := range dateNodes {
				if n.NodeName != "DIV" || n.ChildNodeCount == 0 {
					continue
				}
				dateStr := n.Children[0].NodeValue
				var dt time.Time
				// Handle special days like "Yesterday" and "Today"
				today := time.Now()
				today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
				for i := range 6 {
					dtTmp := today.AddDate(0, 0, -i)
					dayStr := loc.LongDayNames[dtTmp.Weekday()]
					if i == 0 {
						dayStr = loc.Today
					} else if i == 1 {
						dayStr = loc.Yesterday
					}
					if strings.EqualFold(dayStr, dateStr) {
						dt = dtTmp
						break
					}
				}

				if dt == (time.Time{}) {
					var err error
					dt, err = parseDate(dateStr, "", "")
					if err != nil {
						return fmt.Errorf("could not parse date %s: %w", dateStr, err)
					}
				}
				diff := int(dt.Sub(startDate).Hours())
				log.Trace().Msgf("parsed date element %v with distance %d days", dt, diff/24)
				if closestDateNode == nil || absInt(diff) < absInt(closestDateDiff) {
					closestDateNode = n
					closestDateDiff = diff
					knownFirstOccurance = i > 0 || scrollPos <= 0.001
					if knownFirstOccurance {
						break
					}
				}
			}

			if int(closestDateDiff/24) != 0 && matchedNode != nil {
				foundDateNode = matchedNode
				break
			} else if int(closestDateDiff/24) == 0 && closestDateNode != nil {
				if knownFirstOccurance {
					foundDateNode = closestDateNode
					break
				} else {
					matchedNode = closestDateNode
					bisectBounds[1] = (scrollPos + bisectBounds[1]*3) / 4
				}
			} else if closestDateDiff > 0 {
				bisectBounds[0] = scrollPos
			} else if closestDateDiff < 0 {
				bisectBounds[1] = scrollPos
			}

			time.Sleep(50 * time.Millisecond)
		}

		log.Debug().Msgf("final scroll position: %.4f%%", scrollPos*100)

		time.Sleep(1000 * time.Millisecond)

		if foundDateNode == nil {
			return errors.New("could not find -start date")
		}

		for foundDateNode.Parent != nil {
			foundDateNode = foundDateNode.Parent
			if foundDateNode.AttributeValue("style") != "" {
				s.startNodeParent = foundDateNode
				break
			}
		}
	}

	var location string
	if err := chromedp.Location(&location).Do(ctx); err != nil {
		return err
	}
	log.Debug().Msgf("location: %v", location)

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
		log.Trace().Msg("attempting to find first item")
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
		if ok {
			res, err := imageIdFromUrl(photoHref)
			if err == nil {
				firstItem = res
				break
			}
		}
	}
	log.Debug().Msgf("page loaded, most recent item in the feed is: %s", firstItem)
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

	log.Debug().Msg("successfully jumped to the end")

	return nil
}

// doRun runs *runFlag as a command on the given filePath.
func doRun(filePath, imageId string) error {
	if *runFlag == "" {
		return nil
	}
	log.Debug().Msgf("running %v on %v", *runFlag, filePath)
	cmd := exec.Command(*runFlag, filePath, imageId)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func navWithAction(ctx context.Context, action chromedp.Action) error {
	cl := GetContextData(ctx)
	st := time.Now()
	cl.muNavWaiting.Lock()
	cl.listenEvents = true
	cl.muNavWaiting.Unlock()
	action.Do(ctx)
	cl.muNavWaiting.Lock()
	cl.navWaiting = true
	cl.muNavWaiting.Unlock()
	t := time.NewTimer(2 * time.Minute)
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
	log.Debug().Int64("duration", time.Since(st).Milliseconds()).Msgf("navigation done")
	return nil
}

// requestDownloadBackup sends the Shift+D event, to start the download of the currently
// viewed item.
func requestDownloadBackup(ctx context.Context, log zerolog.Logger) error {
	unlock := acquireTabLock(log, "to request download (backup method)")
	defer unlock()
	start := time.Now()

	log.Debug().Msgf("requesting download (backup method)")
	target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID).Do(ctx)
	if err := pressButton(ctx, "D", input.ModifierShift); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	log.Debug().Int64("duration", time.Since(start).Milliseconds()).Msgf("done requesting download (backup method)")
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
		log.Trace().Msgf("triggering button press event: %v, %v, %v", ev.Key, ev.Type, ev.Modifiers)

		if err := chromedp.Run(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// requestDownload clicks the icons to start the download of the currently
// viewed item.
func requestDownload(ctx context.Context, log zerolog.Logger, original bool, hasOriginal *bool) error {
	log.Debug().Msgf("requesting download")
	originalSelector := getAriaLabelSelector(loc.DownloadOriginalLabel)
	var downloadSelector string
	if original {
		downloadSelector = originalSelector
	} else {
		downloadSelector = getAriaLabelSelector(loc.DownloadLabel)
	}

	moreOptionsSelector := getAriaLabelSelector(loc.MoreOptionsLabel)

	foundDownloadButton := false
	i := 0
	for {
		i++
		log := log.With().Int("attempt", i).Logger()
		err := func() error {
			var start time.Time
			defer func() {
				log.Debug().Int64("duration", time.Since(start).Milliseconds()).Msgf("done attempt request download")
			}()

			// unlock := acquireTabLock(log, "to request download")
			// defer unlock()
			start = time.Now()
			log.Trace().Msgf("requesting download")

			// context timeout just in case
			ctxTimeout, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()

			err := chromedp.Run(ctxTimeout,
				target.ActivateTarget(chromedp.FromContext(ctxTimeout).Target.TargetID),
				chromedp.ActionFunc(func(ctx context.Context) error {
					// Wait for more options menu to appear
					if !foundDownloadButton {
						// Open more options dialog
						if err := chromedp.Evaluate(`[...document.querySelectorAll('`+moreOptionsSelector+`')].pop()?.click()`, nil).Do(ctx); err != nil {
							return fmt.Errorf("could not open 'more options' dialog due to %w", err)
						}
					}
					return nil
				}),
				chromedp.Sleep(10*time.Millisecond),
				chromedp.ActionFunc(func(ctx context.Context) error {
					if hasOriginal != nil {
						return chromedp.Evaluate(`!!document.querySelector('`+originalSelector+`')`, hasOriginal).Do(ctx)
					}
					return nil
				}),
				chromedp.SendKeys(downloadSelector, kb.Enter),
			)
			log.Trace().Msgf("done attempting to request download")
			return err
		}()

		if err != nil && (strings.Contains(err.Error(), "Cannot read properties of null (reading 'click')") || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Could not find node with given id (-32000)")) {
			err = errCouldNotPressDownloadButton
		}

		if err == nil {
			log.Debug().Int("triesToSuccess", i).Msgf("download request succeeded")
			break
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else if i >= 3 {
			log.Debug().Msgf("tried to request download %d times, giving up now", i)
			return fmt.Errorf("failed to request download after %d tries, %w, %w", i, errCouldNotPressDownloadButton, err)
		} else if errors.Is(err, errCouldNotPressDownloadButton) || errors.Is(err, context.DeadlineExceeded) {
			log.Debug().Msgf("trying to request download again after error: %v", err)
		} else {
			return fmt.Errorf("encountered error '%s' when requesting download", err.Error())
		}

		time.Sleep(1 * time.Millisecond)
	}

	return nil
}

// navigateToPhoto navigates to the photo page for the given image ID.
func (s *Session) navigateToPhoto(ctx context.Context, log zerolog.Logger, imageId string) error {
	return s.navigateWithAction(ctx, log, chromedp.Navigate(s.getPhotoUrl(imageId)), "to item "+imageId, 10000*time.Millisecond, 5)
}

func (s *Session) navigateWithAction(ctx context.Context, log zerolog.Logger, action chromedp.Action, desc string, timeout time.Duration, retries int) error {
	log.Trace().Msgf("navigating %s", desc)
	var resp *network.Response
	var err error
	for range retries {
		func() {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID)
			resp, err = chromedp.RunResponse(ctx, action)
		}()
		if (err != nil && strings.Contains(err.Error(), "net::ERR_ABORTED")) ||
			(err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil) ||
			(err == nil && (resp != nil && resp.Status == 504)) {
			err = fmt.Errorf("%w: %w", errNavigateAborted, err)
		}
		if errors.Is(err, errNavigateAborted) {
			// retry
			log.Warn().Msgf("error navigating %s: %s (response %v), will try again", desc, err.Error(), resp)
			time.Sleep(100 * time.Millisecond)
		} else {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("error navigating %s: %w", desc, err)
	}
	if resp == nil {
		return nil
	} else if resp.Status == http.StatusOK {
		if err := chromedp.Run(ctx, chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
			return fmt.Errorf("error waiting for body: %w", err)
		}
	} else {
		return fmt.Errorf("unexpected response navigating %s: %v", desc, resp.Status)
	}
	return nil
}

var queuedTabLocks atomic.Int64

func acquireTabLock(log zerolog.Logger, forWhat string) func() {
	if forWhat != "" {
		forWhat = fmt.Sprintf(" %s", forWhat)
	}
	log.Trace().Msgf("acquiring tab lock%s", forWhat)
	start := time.Now()
	queuedTabLocks.Add(1)
	muTabActivity.Lock()
	queuedTabLocks.Add(-1)
	dur := time.Since(start)
	log.Debug().Int64("duration", dur.Milliseconds()).Msgf("acquired tab lock%s", forWhat)
	if dur > 10000*time.Millisecond {
		log.Warn().Int64("duration", dur.Milliseconds()).Msgf("acquiring tab lock%s took %d ms (%d in queue), consider reducing worker count", forWhat, dur.Milliseconds(), queuedTabLocks.Load())
	}
	return muTabActivity.Unlock
}

// getPhotoData gets the date from the currently viewed item.
// First we open the info panel by clicking on the "i" icon (aria-label="Open info")
// if it is not already open. Then we read the date from the
// aria-label="Date taken: ?????" field.
func (s *Session) getPhotoData(ctx context.Context, log zerolog.Logger, imageId string) (PhotoData, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	start := time.Now()

	var filename string
	var dateStr string
	var timeStr string
	var tzStr string

	var n = 0
	log.Debug().Msg("extracting photo data")
	for {
		n++
		start := time.Now()
		log := log.With().Int("attempt", n).Logger()
		if err := func() error {
			unlock := acquireTabLock(log, "to extract photo data")
			defer unlock()
			log.Trace().Msgf("extracting photo data")

			target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID).Do(ctx)

			// If video is 'still processing', photo data may never load, so stop here
			var undownloadable bool
			if err := chromedp.Run(ctx, chromedp.Evaluate(`[...document.querySelectorAll('c-wiz[data-media-key*="'+document.location.href.trim().split('/').pop()+'"]')].filter(x => getComputedStyle(x).visibility != 'hidden')[0]?.textContent.indexOf('Your video will be ready soon') >= 0`, &undownloadable)); err != nil {
				return fmt.Errorf("while checking if video is still processing %w", err)
			}
			if undownloadable {
				return errStillProcessing
			}

			if n%5 == 0 {
				log.Debug().Msgf("getPhotoData: reloading page to force photo info to load")
				if err := s.navigateToPhoto(ctx, log, imageId); err != nil {
					log.Error().Msgf("getPhotoData: %s", err.Error())
				}
			}

			if err := chromedp.Run(ctx,
				chromedp.Sleep(time.Duration(min(2000, (math.Pow(1.5, float64(n-1))-1)*50))*time.Millisecond),
				chromedp.Evaluate(getContentOfFirstVisibleNodeScript(getAriaLabelSelector(loc.FileNameLabel), imageId), &filename),
				chromedp.Evaluate(getContentOfFirstVisibleNodeScript(getAriaLabelSelector(loc.DateLabel), imageId), &dateStr),
				chromedp.Evaluate(getContentOfFirstVisibleNodeScript(getAriaLabelSelector(loc.DateLabel)+" + div "+getAriaLabelSelector(loc.TimeLabel), imageId), &timeStr),
				chromedp.Evaluate(getContentOfFirstVisibleNodeScript(getAriaLabelSelector(loc.DateLabel)+" + div "+getAriaLabelSelector(loc.TzLabel), imageId), &tzStr),
			); err != nil {
				return fmt.Errorf("could not extract photo data due to %w", err)
			}

			if len(dateStr) == 0 && n >= 2 {
				log.Trace().Msgf("incomplete data - Date: %v, Time: %v, Timezone: %v, File name: %v", dateStr, timeStr, tzStr, filename)

				// Click on info button
				log.Debug().Msgf("date not visible, clicking on i button")
				if err := chromedp.Run(ctx,
					target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID),
					chromedp.Sleep(10*time.Millisecond),
					chromedp.EvaluateAsDevTools(`document.querySelector('[data-p*="`+imageId+`"] `+getAriaLabelSelector(loc.OpenInfoMatch)+`')?.click()`, nil)); err != nil {
					return fmt.Errorf("could not click on info button due to %w", err)
				}
			}

			return nil
		}(); err != nil {
			return PhotoData{}, err
		} else if len(filename) > 0 && len(dateStr) > 0 && len(timeStr) > 0 {
			log.Debug().Int64("duration", time.Since(start).Milliseconds()).Int("triesToSuccess", n).Msgf("done finding photo data nodes")
			break
		} else {
			log.Debug().Int64("duration", time.Since(start).Milliseconds()).Msgf("done attempt to find photo data nodes")
		}

		if time.Since(start).Seconds() > 200 {
			return PhotoData{}, fmt.Errorf("timeout waiting for photo info (waited %d ms)", time.Since(start).Milliseconds())
		}

		// Do part of the waiting outside of the tab lock, so we don't hog the active tab the whole time
		time.Sleep(time.Duration(n*500) * time.Millisecond)

		log.Trace().Msgf("failed attempt to find photo data nodes")
	}

	log.Trace().Msgf("parsing date: %v and time: %v", dateStr, timeStr)
	log.Trace().Msgf("parsing filename: %v", filename)
	dt, err := parseDate(dateStr, timeStr, tzStr)
	if err != nil {
		return PhotoData{}, fmt.Errorf("parsing date, %w", err)
	}

	log.Debug().Int64("duration", time.Since(start).Milliseconds()).Msgf("found date and original filename: '%v', '%v'", dt, filename)

	return PhotoData{dt, norm.NFC.String(filename)}, nil
}

// startDownload starts the download of the currently viewed item. It returns
// with an error if the download does not start within a minute.
func (s *Session) startDownload(ctx context.Context, log zerolog.Logger, imageId string, isOriginal bool, hasOriginal *bool, downloadChan <-chan NewDownload) (newDownload NewDownload, progressChan <-chan bool, err error) {
	log.Trace().Msgf("entering startDownload()")

	start := time.Now()

	timeoutTimer := time.NewTimer(120 * time.Second)
	refreshTimer := time.NewTimer(120 * time.Second)
	requestTimer := time.NewTimer(0 * time.Second)

	log.Trace().Msgf("requesting download from tab %s", chromedp.FromContext(ctx).Target.TargetID)

	for {
		// Checking for gphotos warning that this video can't be downloaded (no known solution)
		if err := s.checkForStillProcessing(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return NewDownload{}, nil, fmt.Errorf("error checking for still processing, %w", err)
		}

		select {
		case <-requestTimer.C:
			if err := requestDownload(ctx, log, isOriginal, hasOriginal); err != nil {
				if isOriginal || !errors.Is(err, errCouldNotPressDownloadButton) {
					return NewDownload{}, nil, err
				} else if !isOriginal {
					requestDownloadBackup(ctx, log)
				}
				refreshTimer = time.NewTimer(100 * time.Millisecond)
			} else {
				refreshTimer = time.NewTimer(5 * time.Second)
			}
		case <-refreshTimer.C:
			log.Debug().Msgf("reloading page because download failed to start")
			if err := s.navigateToPhoto(ctx, log, imageId); err != nil {
				log.Error().Msgf("startDownload: %s", err.Error())
				refreshTimer = time.NewTimer(1 * time.Second)
			} else {
				requestTimer = time.NewTimer(100 * time.Millisecond)
			}
		case <-timeoutTimer.C:
			return NewDownload{}, nil, fmt.Errorf("timeout waiting for download to start for %v", imageId)
		case newDownload := <-downloadChan:
			log.Trace().Msgf("downloadChan: %v", newDownload)
			log.Debug().Int64("duration", time.Since(start).Milliseconds()).Str("GUID", newDownload.GUID).Msgf("download started")
			return newDownload, newDownload.progressChan, nil
		default:
			time.Sleep(50 * time.Millisecond)
		}

		log.Trace().Msgf("checking download start status")
	}
}

func (*Session) checkForStillProcessing(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 4000*time.Millisecond)
	defer cancel()
	log.Trace().Msgf("checking for still processing dialog")

	// This text is available before attempting to download, but doesn't show immediately when the page is loaded
	var undownloadable bool
	chromedp.Evaluate(`function () {
		return [...document.querySelectorAll('c-wiz[data-media-key*="document.location.href.trim().split('/').pop()"]')].filter(x => getComputedStyle(x).visibility != 'hidden')[0]?.textContent.indexOf('Your video will be ready soon') >= 0
	}`, &undownloadable).Do(ctx)

	if undownloadable {
		return errStillProcessing
	}

	// The first check only works for requestDownload method (not backup method)
	var nodes []*cdp.Node
	if err := chromedp.Nodes(getAriaLabelSelector(loc.VideoStillProcessingDialogLabel)+` button`, &nodes, chromedp.ByQuery, chromedp.AtLeast(0)).Do(ctx); err != nil {
		return err
	}
	isStillProcessing := len(nodes) > 0
	if isStillProcessing {
		log.Debug().Msgf("found still processing dialog, need to press button to remove")
		// Click the button to close the warning, otherwise it will block navigating to the next photo
		cl := GetContextData(ctx)
		cl.muKbEvents.Lock()
		err := chromedp.MouseClickNode(nodes[0]).Do(ctx)
		cl.muKbEvents.Unlock()
		if err != nil {
			return err
		}
	} else {
		// The second check only works for backup method (not requestDownload)
		if err := chromedp.Evaluate("document.body?.textContent.indexOf('"+loc.VideoStillProcessingStatusText+"') >= 0", &isStillProcessing).Do(ctx); err != nil {
			return err
		}
		if isStillProcessing {
			log.Debug().Msg("found still processing status at bottom of screen, waiting for it to disappear before continuing")
			time.Sleep(5 * time.Second) // Wait for error message to disappear before continuing, otherwise we will also skip next files
		}
		if !isStillProcessing {
			// Sometimes Google returns a different error, check for that too
			if err := chromedp.Evaluate("document.body?.textContent.indexOf('"+loc.NoWebpageFoundText+"') >= 0", &isStillProcessing).Do(ctx); err != nil {
				return err
			}
		}
	}
	if isStillProcessing {
		log.Debug().Msg("received 'Video is still processing' error")
		return errStillProcessing
	}
	return nil
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

// makeOutDir creates a directory in s.downloadDir named of the item ID
func (s *Session) makeOutDir(imageId string) (string, error) {
	newDir := filepath.Join(s.downloadDir, imageId)
	if err := os.MkdirAll(newDir, 0700); err != nil {
		return "", err
	}
	return newDir, nil
}

func (s *Session) waitForDownload(log zerolog.Logger, downloadInfo NewDownload, downloadProgressChan <-chan bool, imageId string) error {
	log = log.With().Str("GUID", downloadInfo.GUID).Logger()

	log.Trace().Msgf("entering waitForDownload")
	downloadTimeout := time.NewTimer(time.Minute)
progressLoop:
	for {
		select {
		case p := <-downloadProgressChan:
			if p {
				// download done
				log.Trace().Msgf("waitForDownload: received download completed message")
				break progressLoop
			} else {
				// still downloading
				log.Trace().Msgf("waitForDownload: received download still in progress message")
				downloadTimeout.Reset(time.Minute)
			}
		case <-downloadTimeout.C:
			return fmt.Errorf("timeout waiting for download to complete for %v", imageId)
		}
	}
	return nil
}

// processDownload creates a directory in s.downloadDir with name = imageId and moves the downloaded files into that directory
func (s *Session) processDownload(log zerolog.Logger, downloadInfo NewDownload, isOriginal, hasOriginal bool, imageId string, data PhotoData) error {
	log.Trace().Msgf("entering processDownload")
	start := time.Now()

	outDir, err := s.makeOutDir(imageId)
	if err != nil {
		return err
	}

	var filePaths []string
	baseNames := []string{}
	if strings.HasSuffix(downloadInfo.suggestedFilename, ".zip") {
		var err error
		filePaths, err = s.handleZip(log, filepath.Join(s.downloadDirTmp, downloadInfo.GUID), outDir)
		if err != nil {
			return err
		}
		foundExpectedFile := false
		for _, f := range filePaths {
			// Google converts files to jpg and gif sometimes, we won't raise an error for those cases
			filename := norm.NFC.String(filepath.Base(f))
			baseNames = append(baseNames, filename)

			foundExpectedFile = foundExpectedFile || compareMangled(data.filename, filename)
		}
		if !foundExpectedFile {
			return fmt.Errorf("expected file %s not found in downloaded zip (found %s) for %s (%w)", data.filename, strings.Join(baseNames, ", "), imageId, errUnexpectedDownload)
		}
	} else {
		var filename string
		if downloadInfo.suggestedFilename != "download" && downloadInfo.suggestedFilename != "" {
			filename = norm.NFC.String(downloadInfo.suggestedFilename)
		} else {
			filename = data.filename
		}

		if isOriginal || !hasOriginal {
			// Error if filename is not the expected filename
			if !compareMangled(data.filename, filename) {
				return fmt.Errorf("expected file %s but downloaded file %s for %s (%w)", data.filename, filename, imageId, errUnexpectedDownload)
			}
		}

		if isOriginal {
			// to ensure the filename is not the same as the other download, change e.g. image_1.jpg to image_1_original.jpg
			ext := filepath.Ext(filename)
			filename = strings.TrimSuffix(filename, ext) + originalSuffix + ext
		}

		newFile := filepath.Join(outDir, filename)
		log.Debug().Msgf("moving %v to %v", downloadInfo.GUID, newFile)
		if err := os.Rename(filepath.Join(s.downloadDirTmp, downloadInfo.GUID), newFile); err != nil {
			return err
		}
		filePaths = []string{newFile}
		baseNames = append(baseNames, filepath.Base(newFile))
	}

	if err := doFileDateUpdate(data.date, filePaths); err != nil {
		return err
	}

	for _, f := range filePaths {
		if err := doRun(f, imageId); err != nil {
			return err
		}
	}

	log.Debug().Int64("duration", time.Since(start).Milliseconds()).Msgf("processed downloaded item")
	log.Info().Msgf("downloaded file(s) %s with date %v", strings.Join(baseNames, ", "), data.date.Format(time.DateOnly))

	return nil
}

// downloadAndProcessItem starts a download then sends it to processDownload for processing
func (s *Session) downloadAndProcessItem(ctx context.Context, log zerolog.Logger, imageId string, newDownloadChan <-chan NewDownload) error {
	log.Trace().Msgf("entering downloadAndProcessItem")
	start := time.Now()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	photoDataChan := make(chan PhotoData, 2)
	errChan := make(chan error)
	jobsRemaining := 3

	go func() {
		log.Trace().Msgf("getting photo data")
		data, err := s.getPhotoData(ctx, log, imageId)
		if err != nil {
			errChan <- err
		} else if fromDate != (time.Time{}) && data.date.Before(fromDate) {
			errChan <- errPhotoTakenBeforeFromDate
		} else if toDate != (time.Time{}) && data.date.After(toDate) {
			errChan <- errPhotoTakenAfterToDate
		} else {
			errChan <- nil

			// we need two of these in case we are downloading an original
			photoDataChan <- data
			photoDataChan <- data
		}
	}()

	startDownloadMu := sync.Mutex{}
	hasOriginalChan := make(chan bool, 1)

	doDownload := func(isOriginal bool) {
		start := time.Now()
		log := log.With().Bool("isOriginal", isOriginal).Logger()
		var hasOriginal bool
		hasOriginalPtr := &hasOriginal
		if isOriginal {
			hasOriginalPtr = nil
			hasOriginal = true
		}
		var photoData PhotoData
		var err error
		for i := range 3 {
			var downloadInfo NewDownload
			var downloadProgressChan <-chan bool
			startDownloadMu.Lock()
			downloadInfo, downloadProgressChan, err = s.startDownload(ctx, log, imageId, isOriginal, hasOriginalPtr, newDownloadChan)
			startDownloadMu.Unlock()
			if i == 0 && !isOriginal {
				hasOriginalChan <- hasOriginal
			}
			if err != nil {
				log.Trace().Msgf("download failed: %v", err)
				break
			} else {
				err = s.waitForDownload(log, downloadInfo, downloadProgressChan, imageId)
				if err != nil {
					break
				}

				log.Trace().Msgf("download completed, will continue processing when photo data is ready")
				if photoData == (PhotoData{}) {
					photoData = <-photoDataChan
				}
				err = s.processDownload(log, downloadInfo, isOriginal, hasOriginal, imageId, photoData)
				if errors.Is(err, errUnexpectedDownload) {
					log.Err(err).Msgf("error processing download for %s (try %d/3)", imageId, i+1)
					continue
				}
				log.Debug().Int64("duration", time.Since(start).Milliseconds()).Msgf("doDownload done")
				break
			}
		}
		errChan <- err
	}

	go doDownload(false)
	if <-hasOriginalChan {
		go doDownload(true)
	} else {
		jobsRemaining--
	}

	go func() {
		deadline := time.NewTimer(30 * time.Minute)
		for {
			select {
			case <-ctx.Done():
				errChan <- ctx.Err()
				return
			case <-time.After(60 * time.Second):
				log.Trace().Msgf("downloadAndProcessItem: waiting for %d jobs to finish", jobsRemaining)
			case <-deadline.C:
				errChan <- fmt.Errorf("downloadAndProcessItem: timeout waiting for %d jobs for %s", jobsRemaining, imageId)
				return
			}
		}
	}()

	for err := range errChan {
		jobsRemaining--
		if err != nil {
			log.Trace().Msgf("downloadAndProcessItem: job result %s, %d jobs remaining", err.Error(), jobsRemaining)
			if !errors.Is(err, errStillProcessing) {
				if !errors.Is(err, context.Canceled) {
					captureScreenshot(ctx, filepath.Join(s.downloadDir, "error"))
				}

				log.Info().Msgf("unrecoverable error occurred during download, removing files already downloaded for this item")
				// Error downloading original or generated image, remove files already downloaded
				if err := os.RemoveAll(filepath.Join(s.downloadDir, imageId)); err != nil {
					log.Err(err).Msgf("error removing files already downloaded: %v", err)
				}
			}

			return err
		} else {
			log.Trace().Msgf("downloadAndProcessItem: job result done, %d jobs remaining", jobsRemaining)
		}
		if jobsRemaining == 0 {
			log.Debug().Int64("duration", time.Since(start).Milliseconds()).Msgf("downloadAndProcessItem successfully completed")
			return nil
		}
	}

	// If we get here, the channel was closed but jobsRemaining > 0
	return ctx.Err()
}

// handleZip handles the case where the currently item is a zip file. It extracts
// each file in the zip file to the same folder, and then deletes the zip file.
func (s *Session) handleZip(log zerolog.Logger, zipfile, outFolder string) ([]string, error) {
	st := time.Now()
	log.Debug().Msgf("unzipping %v to %v", zipfile, outFolder)
	// unzip the file
	files, err := zip.Unzip(zipfile, outFolder)
	if err != nil {
		return []string{""}, err
	}

	// delete the zip file
	if err := os.Remove(zipfile); err != nil {
		return []string{""}, err
	}

	log.Debug().Int64("duration", time.Since(st).Milliseconds()).Msgf("done unzipping downloaded zip file: %s", zipfile)
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

func GetContextData(ctx context.Context) ContextLocksPointer {
	return ctx.Value(contextLocksKey).(ContextLocksPointer)
}

func SetContextData(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextLocksKey, &ContextLocks{
		muNavWaiting: sync.RWMutex{},
		muKbEvents:   sync.Mutex{},
		listenEvents: false,
		navWaiting:   false,
		navDone:      make(chan bool, 1),
	})
}

func listenNavEvents(ctx context.Context) {
	cl := GetContextData(ctx)
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
					time.Sleep(10 * time.Millisecond)
				}
			}()
		}
	})
}

func setScrollPosition(ctx context.Context, pos float64) error {
	var mainSel string
	if len(*albumIdFlag) > 1 {
		mainSel = `c-wiz c-wiz c-wiz`
	} else {
		mainSel = `[role="main"]`
	}

	if err := chromedp.Evaluate(fmt.Sprintf(`
		(function() {
			var main = [...document.querySelectorAll('%s')].filter(x => x.querySelector('a[href*="/photo/"]') && getComputedStyle(x).visibility != 'hidden')[0];
			const scrollTarget = %f;
			main.scrollTo(0, main.scrollHeight*scrollTarget);
		})();
	`, mainSel, pos), nil).Do(ctx); err != nil {
		return err
	}
	return nil
}

func getScrollPosition(ctx context.Context, sliderPos *float64) error {
	var mainSel string
	if len(*albumIdFlag) > 1 {
		mainSel = `c-wiz c-wiz c-wiz`
	} else {
		mainSel = `[role="main"]`
	}

	var err error
	for range 3 {
		func() {
			ctx, cancel := context.WithTimeout(ctx, 4000*time.Millisecond)
			defer cancel()
			if err != nil {
				unlock := acquireTabLock(log.Logger, "getScrollPosition")
				defer unlock()
				target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID).Do(ctx)
			}
			err = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`
				(function() {
					var main = [...document.querySelectorAll('%s')].filter(x => x.querySelector('a[href*="/photo/"]') && getComputedStyle(x).visibility != 'hidden')[0];
					return main ? (main.scrollTop+0.000001)/(main.scrollHeight-main.clientHeight+0.000001) : 0.0;
				})()`, mainSel), &sliderPos))
		}()
		if err == nil {
			break
		}
		log.Warn().Err(err).Msgf("error getting scroll position")
	}

	return err
}

// Resync the library/album of photos
// Use [...document.querySelectorAll('a[href^=".{relPath}photo/"]')] to find all visible photos
// Check that each one is already downloaded. Optionally check/update date from the element
// attr, e.g. aria-label="Photo - Landscape - Feb 12, 2025, 6:34:39 PM"
// Then do .pop().focus() on the last a element found to scroll to it and make more photos visible
// Then repeat until we get to the end
// If any photos are missing we can asynchronously create a new chromedp context, then in that
// context navigate to that photo and call downloadAndProcessItem
func (s *Session) resync(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	listenNavEvents(ctx)

	lastNode := &cdp.Node{}
	var nodes []*cdp.Node
	i := 0                         // next node to process in nodes array
	n := 0                         // number of nodes processed in all
	var newItemsCount atomic.Int64 // number of photos downloaded
	retries := 0                   // number of subsequent failed attempts to find new items to download
	sliderPos := 0.0
	estimatedRemaining := 1000
	photoNodeSelector := s.getPhotoNodeSelector()

	log.Trace().Msgf("finding start node")
	opts := []chromedp.QueryOption{chromedp.ByQuery, chromedp.AtLeast(0)}

	if err := chromedp.Nodes(photoNodeSelector, &nodes, opts...).Do(ctx); err != nil {
		return fmt.Errorf("error finding photo nodes, %w", err)
	}
	if len(nodes) == 0 {
		log.Info().Msg("no photos to sync")
		return nil
	}

	jobChan := make(chan Job)
	resultChan := make(chan string, *workersFlag)
	errChan := make(chan error, *workersFlag)
	var runningWorkers atomic.Int64
	runningWorkers.Store(*workersFlag)
	var workerDownloadChanByFrameId sync.Map
	for i := range *workersFlag {
		workerDownloadChan := make(chan NewDownload, 1)
		workerDownloadChanByFrameId.Store(s.downloadWorker(int(i+1), jobChan, resultChan, errChan, workerDownloadChan), workerDownloadChan)
	}

	// job channel routing
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case newDownload := <-s.newDownloadChan:
				worker, exists := workerDownloadChanByFrameId.Load(newDownload.targetId)
				if !exists {
					s.globalErrChan <- fmt.Errorf("worker with targetId %s not found for download of %s", newDownload.targetId, newDownload.suggestedFilename)
					return
				}
				go func() {
					worker.(chan NewDownload) <- newDownload
				}()
			case res := <-resultChan:
				if res != "" {
					if _, exists := s.downloadedItems.Load(res); exists {
						log.Warn().Msgf("we've downloaded the same item twice, this shouldn't happen")
					} else {
						s.downloadedItems.Store(res, struct{}{})
					}
				} else {
					s.skippedCount.Add(1)
				}
			case err := <-errChan:
				if err != nil {
					log.Trace().Msgf("received error from worker: %s", err.Error())
					s.globalErrChan <- err
				}
				if runningWorkers.Add(-1) == 0 {
					s.globalErrChan <- nil
					return
				}
			}
		}
	}(ctx)

	// progress logger
	go func(ctx context.Context) {
		lastSyncedCount := 0
		iterationsWithNoProgressCount := 0
		start := time.Now()
		for {
			done := false
			select {
			case <-time.After(60 * time.Second):
			case <-ctx.Done():
				done = true
			}
			downloadedCount := 0
			s.downloadedItems.Range(func(key, value interface{}) bool {
				downloadedCount++
				return true
			})
			syncedCount := n
			progress := min(float64(syncedCount)/float64(estimatedRemaining+syncedCount), 1)
			if !done {
				queueCount := newItemsCount.Load() - int64(downloadedCount) - int64(s.skippedCount.Load())
				totalCount := syncedCount + estimatedRemaining
				timeRemaining := time.Since(start) * time.Duration(estimatedRemaining) / time.Duration(max(1, syncedCount-int(queueCount/2)))
				log.Info().Msgf("so far: downloaded %d (%d in queue), progress: %.2f%% (%d/%d), estimated remaining: %d (%s)", downloadedCount, queueCount, progress*100, syncedCount, totalCount, estimatedRemaining, timeRemaining.Round(time.Second))
			} else {
				log.Info().Msgf("in total: synced %v items, downloaded %v, progress: %.2f%%", syncedCount, downloadedCount, progress*100)
				return
			}
			if syncedCount == lastSyncedCount {
				iterationsWithNoProgressCount++
				if iterationsWithNoProgressCount > 20 {
					panic("no new items processed for 20 minutes, stopping sync")
				}
			} else {
				iterationsWithNoProgressCount = 0
			}
			lastSyncedCount = syncedCount
		}
	}(ctx)

syncAllLoop:
	for {
		if retries%5 == 0 {
			target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID).Do(ctx)
			if retries != 0 {
				log.Trace().Msgf("we seem to be stuck, manually scrolling might help")
				if err := doActionWithTimeout(ctx, chromedp.KeyEvent(kb.ArrowDown), 1000*time.Millisecond); err != nil {
					log.Warn().Err(err).Msgf("error scrolling page down manually, %v", err)
				}
				time.Sleep(200 * time.Millisecond)
			}
		}

		if retries > 0 && retries%10 == 0 {
			// loading slow, let's give it some extra time
			time.Sleep(1 * time.Second)
		}

		// New new nodes found, does it look like we are done?
		if retries > 5000 || (retries > 100 && estimatedRemaining < 50) {
			break
		}

		if scrollErr := getScrollPosition(ctx, &sliderPos); scrollErr != nil {
			// sometimes chromedp gets into a bad state here, so let's restart navigation and try again
			if err := s.navigateWithAction(ctx, log.Logger, chromedp.Navigate(gphotosUrl+s.userPath+s.albumPath), "to start", 20000*time.Millisecond, 5); err != nil {
				return fmt.Errorf("error getting slider position, %w, followed by error when attempting to recover, %v", scrollErr, err)
			}
			chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
			if err := setScrollPosition(ctx, sliderPos); err != nil {
				captureScreenshot(ctx, filepath.Join(s.downloadDir, "error"))
				return fmt.Errorf("error getting slider position, %w, followed by error when attempting to recover, %v", scrollErr, err)
			}
		}
		log.Trace().Msgf("slider position: %.2f%%", sliderPos*100)

		select {
		case err := <-s.globalErrChan:
			if errors.Is(err, errPhotoTakenBeforeFromDate) {
				log.Info().Msg("found photo taken before -from date, stopping sync here")
				break syncAllLoop
			}
			return err
		default:
		}

		if n < 5 || sliderPos < 0.001 {
			estimatedRemaining = 50
		} else {
			estimatedRemaining = int(math.Floor((1/sliderPos - 1) * float64(n+30)))
		}

		if n != 0 && i >= len(nodes) {
			if retries == 0 {
				// start by scrolling to the next batch by focusing the last processed node
				log.Trace().Msgf("scrolling to last processed node: %v", lastNode.NodeID)
				if err := doActionWithTimeout(ctx, dom.Focus().WithNodeID(lastNode.NodeID), 1000*time.Millisecond); err != nil {
					log.Debug().Msgf("error scrolling to next batch of items: %v", err)
				}
			}

			if err := chromedp.Nodes(photoNodeSelector, &nodes, chromedp.ByQueryAll, chromedp.AtLeast(0)).Do(ctx); err != nil {
				return fmt.Errorf("error finding photo nodes, %w", err)
			}
			log.Trace().Msgf("found %d items, checking if any are new", len(nodes))

			// remove already processed nodes
			foundNodes := len(nodes)
			for i, node := range nodes {
				if node == lastNode {
					nodes = nodes[i+1:]
					break
				}
			}
			if len(nodes) == 0 {
				retries++
				continue
			}
			log.Trace().Msgf("%d nodes on page, processing %d that haven't been processed yet", foundNodes, len(nodes))
			if foundNodes == len(nodes) {
				log.Warn().Msg("only new nodes found, expected an overlap")
			}

			retries = 0
			i = 0
		}

		imageIds := []string{}
		foundUntil := false

		for i < len(nodes) && (*batchSizeFlag <= 0 || len(imageIds) < *batchSizeFlag) {
			lastNode = nodes[i]
			i++
			n++

			imageId, err := imageIdFromUrl(lastNode.AttributeValue("href"))
			if err != nil {
				return fmt.Errorf("error getting item id from url, %w", err)
			}

			if strings.EqualFold(imageId, *untilFlag) {
				foundUntil = true
				break
			}

			log := log.With().Str("itemId", imageId).Logger()

			shouldDownload, err := s.isNewItem(log, imageId, false)
			if err != nil {
				return err
			} else if !shouldDownload {
				if len(imageIds) > 0 {
					break
				} else {
					continue
				}
			}

			imageIds = append(imageIds, imageId)
		}

		if len(imageIds) > 0 {
			log.Debug().Msgf("adding %d items to queue", len(imageIds))
			job := Job{imageIds}

			log.Trace().Msgf("queuing job with itemIds: %s", strings.Join(job.imageIds, ", "))

			select {
			case err := <-s.globalErrChan:
				if errors.Is(err, errPhotoTakenBeforeFromDate) {
					log.Info().Msg("found photo taken before -from date, stopping sync here")
					break syncAllLoop
				}
				return err
			case jobChan <- job:
				log.Trace().Msgf("queued job with itemIds: %s", strings.Join(job.imageIds, ", "))
			}
		}

		newItemsCount.Add(int64(len(imageIds)))

		if foundUntil {
			break
		}
	}
	close(jobChan)

	for err := range s.globalErrChan {
		if !errors.Is(err, errPhotoTakenBeforeFromDate) {
			return err
		}
	}
	return nil
}

func (s *Session) isNewItem(log zerolog.Logger, imageId string, markFound bool) (bool, error) {
	if _, exists := s.foundItems.Load(imageId); exists {
		return false, nil
	}

	isNew := true
	hasFiles, err := s.dirHasFiles(imageId)
	if err != nil {
		return false, err
	} else if hasFiles {
		log.Trace().Msgf("skipping item, already downloaded")
		isNew = false
	}

	if markFound || !isNew {
		s.foundItems.Store(imageId, struct{}{})
	}

	return isNew, nil
}

func (s *Session) getPhotoUrl(imageId string) string {
	return gphotosUrl + s.userPath + s.albumPath + "/photo/" + imageId
}

func (s *Session) downloadWorker(workerId int, jobs <-chan Job, resultChan chan<- string, errChan chan<- error, downloadChan <-chan NewDownload) string {
	ctx, cancel := chromedp.NewContext(s.parentContext)
	chromedp.Run(ctx)
	ctx = SetContextData(ctx)
	listenNavEvents(ctx)

	log := log.With().Int("workerId", workerId).Logger()
	go func() {
		defer cancel()
		for job := range jobs {
			log.Debug().Msgf("worker received batch of %d items", len(job.imageIds))
			log.Trace().Msgf("starting job with itemIds: %s", strings.Join(job.imageIds, ", "))
			isConsecutive := false
			for i, imageId := range job.imageIds {
				log := log.With().Str("itemId", imageId).Int("batchItemIndex", i).Logger()

				log.Trace().Msgf("processing batch item %d", i)
				downloadedItemId, err := s.doWorkerBatchItem(ctx, log, imageId, downloadChan, isConsecutive)
				isConsecutive = true
				if errors.Is(err, errAbortBatch) {
					break
				} else if errors.Is(err, errAlreadyDownloaded) || errors.Is(err, errStillProcessing) {
					if errors.Is(err, errStillProcessing) {
						// Old highlight videos are no longer available
						log.Info().Msg("skipping generated highlight video that Google cannot be downloaded")
						isConsecutive = false
					}
					downloadedItemId = ""
				} else if err != nil {
					errChan <- err
					return
				}
				resultChan <- downloadedItemId
			}
			log.Debug().Msgf("worker finished processing batch of %d items", len(job.imageIds))
		}
		errChan <- nil
	}()
	return chromedp.FromContext(ctx).Target.TargetID.String()
}

func (s *Session) doWorkerBatchItem(ctx context.Context, log zerolog.Logger, imageId string, downloadChan <-chan NewDownload, isConsecutive bool) (string, error) {
	expectedLocation := s.getPhotoUrl(imageId)

	var previousLocation string
	if err := chromedp.Run(ctx, chromedp.Location(&previousLocation)); err != nil {
		return "", fmt.Errorf("error getting location: %w", err)
	}
	log.Trace().Msgf("current location: %s", previousLocation)
	atExpectedUrl := previousLocation == expectedLocation
	if isConsecutive && !atExpectedUrl && strings.HasPrefix(previousLocation, gphotosUrl) {
		var location string
		// pressing right arrow to navigate to the next item (batch jobs should be sequential)
		log.Trace().Msgf("navigating to next item by right arrow press (%s)", expectedLocation)
		err := chromedp.Run(ctx,
			target.ActivateTarget(chromedp.FromContext(ctx).Target.TargetID),
			chromedp.ActionFunc(s.navRight(log)),
			chromedp.Sleep(10*time.Millisecond),
			chromedp.Location(&location),
			chromedp.ActionFunc(
				func(ctx context.Context) error {
					if location != expectedLocation {
						log.Warn().Msgf("after nav to right, expected location %s, got %s", expectedLocation, location)
					} else {
						atExpectedUrl = true
					}
					return nil
				},
			),
		)
		if err != nil {
			log.Error().Msgf("error navigating to next batch item: %s", err.Error())
		} else if atExpectedUrl {
			log.Trace().Msgf("done navigating to %v by right arrow press", expectedLocation)
		}
	}

	if !atExpectedUrl {
		log.Trace().Msgf("navigating to expected location")
		if err := s.navigateToPhoto(ctx, log, imageId); err != nil {
			return "", err
		}
	}

	time.Sleep(2 * time.Millisecond)

	isNew, err := s.isNewItem(log, imageId, true)
	if err != nil {
		return "", err
	} else if !isNew {
		return "", errAlreadyDownloaded
	}

	log.Debug().Msgf(`item not found in photos dir, downloading it now`)

	err = chromedp.Run(ctx, chromedp.ActionFunc(
		func(ctx context.Context) error {
			return s.downloadAndProcessItem(ctx, log, imageId, downloadChan)
		},
	))
	if errors.Is(err, errPhotoTakenAfterToDate) {
		log.Warn().Msg("skipping photo taken after -to date. If you see many of these messages, something has gone wrong.")
	} else if err != nil {
		log.Trace().Msgf("downloadWorker: encountered error while processing batch item: %s", err.Error())
		return "", err
	} else {
		return imageId, nil
	}
	return "", nil
}

// navRight navigates to the next item to the right
func (s *Session) navRight(log zerolog.Logger) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		log.Debug().Msg("Navigating right")
		return navWithAction(ctx, chromedp.KeyEvent(kb.ArrowRight))
	}
}

// Check if there are folders in the download dir that were not seen in gphotos
func (s *Session) checkForRemovedFiles(ctx context.Context) error {
	if *removedFlag {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()

		log.Info().Msg("checking for removed files")
		deleted := []string{}
		s.existingItems.Range(func(itemId, _ any) bool {
			if itemId != "tmp" {
				// Check if the folder name is in the map of photo IDs
				if _, exists := s.foundItems.Load(itemId); !exists {
					deleted = append(deleted, itemId.(string))
				}
			}
			return true
		})
		if len(deleted) > 0 {
			log.Info().Msgf("folders found for %d local photos that were not found in this sync. Checking google photos to confirm they are not there", len(deleted))
		}
		i := 0
		for i < len(deleted) {
			imageId := deleted[i]
			var resp int
			if err := chromedp.Run(ctx,
				chromedp.Evaluate(`new Promise((res) => fetch('`+s.getPhotoUrl(imageId)+`').then(x => res(x.status)));`, &resp,
					func(p *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
						return p.WithAwaitPromise(true)
					}),
			); err != nil {
				log.Err(err).Msgf("error checking for removed file %s: %s, will not continue checking for removed files", imageId, err.Error())
				return nil
			}
			if resp == http.StatusOK {
				log.Debug().Msgf("photo %s was not in original sync, but is still present on google photos, it might be in the trash", imageId)
				deleted = slices.Delete(deleted, i, i+1)
				continue
			} else if resp == http.StatusNotFound {
				log.Trace().Msgf("photo %s not found on google photos, but is in local folder, it was probably deleted or removed from album", imageId)
			} else {
				return fmt.Errorf("unexpected response for %s: %v", imageId, resp)
			}
			i++
		}
		if len(deleted) > 0 {
			log.Info().Msgf("folders found for %d local photos that don't exist on google photos (in album if using -album), list saved to .removed", len(deleted))
			if err := os.WriteFile(path.Join(s.downloadDir, ".removed"), []byte(strings.Join(deleted, "\n")), 0644); err != nil {
				return err
			}
		}
	}
	return nil
}

// doFileDateUpdate updates the file date of the downloaded files to the photo date
func doFileDateUpdate(date time.Time, filePaths []string) error {
	log.Debug().Msgf("setting file date for %v", filePaths)

	for _, f := range filePaths {
		if err := setFileDate(f, date); err != nil {
			return err
		}
	}

	return nil
}

func doActionWithTimeout(ctx context.Context, action chromedp.Action, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := action.Do(ctx); err != nil {
		return err
	}
	return nil
}

// Sets modified date of file to given date
func setFileDate(filepath string, date time.Time) error {
	if err := os.Chtimes(filepath, date, date); err != nil {
		return err
	}
	return nil
}

func startDownloadListener(ctx context.Context, newDownloadChan chan NewDownload) {
	currentDownloads := make(map[string]chan bool)

	// Listen for new download events
	chromedp.ListenBrowser(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadWillBegin); ok {
			log.Debug().Str("GUID", ev.GUID).Msgf("download of %s started", ev.SuggestedFilename)
			if ev.SuggestedFilename == "downloads.html" {
				return
			}
			if _, exists := currentDownloads[ev.GUID]; !exists {
				currentDownloads[ev.GUID] = make(chan bool)
			}
			go func() {
				newDownloadChan <- NewDownload{ev.GUID, ev.SuggestedFilename, ev.FrameID.String(), currentDownloads[ev.GUID]}
			}()
		}
	})

	// Listen for download progress events
	chromedp.ListenBrowser(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadProgress); ok {
			if ev.State == browser.DownloadProgressStateInProgress {
				select {
				case currentDownloads[ev.GUID] <- false:
				default:
				}
			}
			if ev.State == browser.DownloadProgressStateCompleted {
				log.Trace().Str("GUID", ev.GUID).Msgf("received download completed event")
				progressChan := currentDownloads[ev.GUID]
				delete(currentDownloads, ev.GUID)
				go func() {
					time.Sleep(1 * time.Millisecond)
					progressChan <- true
				}()
			}
		}
	})
}

func getContentOfFirstVisibleNodeScript(sel string, imageId string) string {
	return fmt.Sprintf(`[...document.querySelectorAll('[data-p*="%s"] %s')].filter(x => x.checkVisibility()).map(x => x.textContent)[0] || ''`, imageId, sel)
}

func (s *Session) dirHasFiles(imageId string) (bool, error) {
	if _, exists := s.existingItems.Load(imageId); !exists {
		return false, nil
	}
	entries, err := os.ReadDir(filepath.Join(s.downloadDir, imageId))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, v := range entries {
		if !v.IsDir() {
			f, err := os.Stat(filepath.Join(s.downloadDir, imageId, v.Name()))
			if err != nil {
				return false, err
			}
			if f.Size() > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Session) getPhotoNodeSelector() string {
	return fmt.Sprintf(`a[href^=".%s/photo/"]`, s.albumPath)
}

// compiled year regex
var yearRegex = regexp.MustCompile(`\d{4}`)
var dayRegex = regexp.MustCompile(`\d{1,2}`)
var timeRegex = regexp.MustCompile(`(\d{1,2}):(\d\d)(?::\d\d)?.?([aApP][Mm])?$`)
var timeZoneRegex = regexp.MustCompile(`GMT([-+])?(\d{1,2})(?::(\d\d))?`)

func parseDate(dateStr, timeStr, tzStr string) (time.Time, error) {
	var year, month, day, hour, minute int
	yearStr := yearRegex.FindString(dateStr)
	if yearStr != "" {
		year, _ = strconv.Atoi(yearStr)
		dateStr = strings.Replace(dateStr, yearStr, "", 1)
	} else {
		year = time.Now().Year()
	}
	log.Trace().Msgf("parsed year: %d, dateStr: %s", year, dateStr)

	dayStr := dayRegex.FindString(dateStr)
	if dayStr != "" {
		day, _ = strconv.Atoi(dayStr)
	}
	dateStr = strings.Replace(dateStr, dayStr, "", 1)

	for i, v := range loc.ShortMonthNames {
		if strings.Contains(strings.ToUpper(dateStr), strings.ToUpper(v)) {
			month = i + 1
			break
		}
	}
	if month == 0 {
		return time.Time{}, fmt.Errorf("could not find month in string %s", dateStr)
	}
	log.Trace().Msgf("parsed month: %d, dateStr: %s", month, dateStr)

	if timeStr != "" {
		timeMatch := timeRegex.FindStringSubmatch(timeStr)
		if timeMatch == nil {
			return time.Time{}, fmt.Errorf("could not find time in string %s", timeStr)
		}
		hour, _ = strconv.Atoi(timeMatch[1])
		minute, _ = strconv.Atoi(timeMatch[2])
		if strings.EqualFold(timeMatch[3], "pm") && hour < 12 {
			hour += 12
		}
		if strings.EqualFold(timeMatch[3], "am") && hour == 12 {
			hour = 0
		}
	}

	// read time zone from timezoneStr in format GMT-05:00
	var timeZone *time.Location
	timeZoneStr := strings.Trim(tzStr, " ")
	if timeZoneStr != "" {
		timeZoneMatch := timeZoneRegex.FindStringSubmatch(timeZoneStr)
		if timeZoneMatch != nil {
			tzHour, _ := strconv.Atoi(timeZoneMatch[2])
			tzMinute, _ := strconv.Atoi(timeZoneMatch[3])
			offset := tzHour*60*60 + tzMinute*60
			if timeZoneMatch[1] == "-" {
				offset = -offset
			}
			timeZone = time.FixedZone("", offset)
		} else {
			return time.Time{}, fmt.Errorf("could not parse time zone in string %s", timeZoneStr)
		}
	} else {
		timeZone = time.Local
	}
	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, timeZone), nil
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Compare two file names, where s2 is sometimes mangled by gphotos
// there will be some false positives, this is ok
func compareMangled(s1, s2 string) bool {
	// URL-decoding s1 since Google Photos may return URL-encoded filenames
	if decoded, err := url.QueryUnescape(s1); err == nil {
		s1 = decoded
	}
	
	sr1 := []rune(s1)
	sr2 := []rune(s2)

	l1 := len(sr1)
	for i := range slices.Backward(sr1) {
		if sr1[i] == '.' {
			l1 = i + 1
			break
		}
	}

	l2 := len(sr2)
	for i := range slices.Backward(sr2) {
		if sr2[i] == '.' {
			l2 = i + 1
			break
		}
	}

	i1 := 0
	for i1 < len(sr1) && sr1[i1] == '.' && sr2[i1] != '.' {
		i1++
	}

	for i2 := range l2 {
		if i1 >= len(sr1) {
			return i2 == l2-1 && sr2[i2] == '.'
		}
		if sr1[i1] != sr2[i2] && sr2[i2] != '_' {
			return false
		}
		i1++
	}

	return i1 >= l1
}
