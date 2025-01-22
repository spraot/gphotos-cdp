package main

import (
	"context"
	"errors"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"github.com/rs/zerolog/log"
)

type DownloadJob struct {
	st                time.Time
	location          string
	imageId           string
	downloadDone      chan bool
	done              chan bool
	err               chan error
	dlFile            string
	suggestedFilename string
	storedFiles       []string
	timeTaken         chan time.Time
	originalFilename  chan string
}

type Download struct {
	startTime         time.Time
	suggestedFilename string
	filename          string
	success           chan bool
	done              chan error
	dlTimeout         *time.Timer
}

type DownloadManager struct {
	jobs        chan *DownloadJob
	maxWorkers  int
	session     *Session
	ctx         context.Context
	newDownload chan Download
	downloads   map[string]Download
}

func NewDownloadManager(ctx context.Context, session *Session, maxWorkers int) *DownloadManager {
	dm := &DownloadManager{
		jobs:        make(chan *DownloadJob, maxWorkers),
		maxWorkers:  maxWorkers,
		session:     session,
		ctx:         ctx,
		newDownload: make(chan Download),
		downloads:   make(map[string]Download),
	}

	log.Debug().Msg("Starting download manager")
	chromedp.ListenTarget(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadWillBegin); ok {
			log.Trace().Msgf("Event: Download of %s started", ev.SuggestedFilename)
			download := Download{
				startTime:         time.Now(),
				suggestedFilename: ev.SuggestedFilename,
				filename:          ev.GUID,
				success:           make(chan bool),
				done:              make(chan error),
				dlTimeout:         time.NewTimer(60 * time.Second),
			}
			log.Trace().Msgf("Sending download of %s to worker", ev.SuggestedFilename)
			dm.downloads[ev.GUID] = download
			dm.newDownload <- download
		}
	})

	chromedp.ListenTarget(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadProgress); ok {
			dl := dm.downloads[ev.GUID]
			log.Trace().Msgf("Event: Download of %s progress: %.2f%%", dl.suggestedFilename, (ev.ReceivedBytes/ev.TotalBytes)*100)
			if ev.State == browser.DownloadProgressStateCompleted {
				dlTime := time.Since(dl.startTime).Milliseconds()
				dlMb := ev.ReceivedBytes / 1024 / 1024
				log.Debug().Msgf("Download of %s completed, downloaded %.2fMB in %dms (%.2fMB/s)",
					ev.GUID,
					dlMb,
					dlTime,
					dlMb/float64(dlTime)*1000,
				)
				dl.success <- true
				dl.done <- nil
				dl.dlTimeout.Reset(60 * time.Second)
				delete(dm.downloads, ev.GUID)
			}
			if ev.State == browser.DownloadProgressStateCanceled {
				log.Debug().Msgf("Download of %s cancelled", ev.GUID)
				dl.done <- errors.New("download cancelled")
				delete(dm.downloads, ev.GUID)
			}
		}
	})

	// Start worker goroutines
	for i := 0; i < maxWorkers; i++ {
		go dm.worker()
	}

	return dm
}

func (dm *DownloadManager) worker() {
	for job := range dm.jobs {
		err := dm.session.dlAndMove(dm.ctx, job)
		if err != nil {
			job.err <- err
			continue
		}

		// Update file dates before moving
		if err := dm.session.doFileDateUpdate(dm.ctx, job); err != nil {
			job.err <- err
			continue
		}

		// Run the command on downloaded files
		for _, f := range job.storedFiles {
			if err := doRun(f); err != nil {
				job.err <- err
				continue
			}
		}

		job.done <- true
	}
}

func (dm *DownloadManager) StartDownload(location, imageId string, readyForNext chan bool) (*DownloadJob, error) {
	job := &DownloadJob{
		st:               time.Now(),
		location:         location,
		imageId:          imageId,
		timeTaken:        make(chan time.Time, 1),
		originalFilename: make(chan string, 1),
		done:             make(chan bool, 1),
		err:              make(chan error, 1),
	}

	log.Debug().Msgf("Starting download of %s", imageId)

	// Start download in the background
	if err := startDownload(dm.ctx); err != nil {
		return nil, err
	}

	go func() {
		dlStartTimeout := time.NewTimer(60 * time.Second)
		defer dlStartTimeout.Stop()

		// for {
		select {
		case download := <-dm.newDownload:
			log.Debug().Msgf("Download of %s started in %dms", download.suggestedFilename, time.Since(job.st).Milliseconds())
			job.dlFile = download.filename
			job.suggestedFilename = download.suggestedFilename
			job.downloadDone = download.success
			dm.jobs <- job
			readyForNext <- true

			go func() {
				select {
				case err := <-download.done:
					download.dlTimeout.Stop()
					if err != nil {
						job.err <- err
					}
					return
				case <-download.dlTimeout.C:
					job.err <- errors.New("timeout waiting for download progress")
					return
				}
			}()
			return
		case <-dlStartTimeout.C:
			job.err <- errors.New("timeout waiting for download to start")
			readyForNext <- true
			return
		}
		// }
	}()

	return job, nil
}
