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
}

type Download struct {
	suggestedFilename string
	filename          string
	done              chan bool
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
			done := make(chan bool)
			download := Download{
				suggestedFilename: ev.SuggestedFilename,
				filename:          ev.GUID,
				done:              done,
			}
			log.Trace().Msgf("Sending download of %s to worker", ev.SuggestedFilename)
			dm.newDownload <- download
			dm.downloads[ev.GUID] = download
		}
	})

	chromedp.ListenTarget(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadProgress); ok {
			log.Trace().Msgf("Event: Download of %s progress: %.2f%%", dm.downloads[ev.GUID].suggestedFilename, (ev.ReceivedBytes/ev.TotalBytes)*100)
			if ev.State == browser.DownloadProgressStateCompleted {
				log.Debug().Msgf("Download of %s completed", ev.GUID)
				dm.downloads[ev.GUID].done <- true
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
		st:        time.Now(),
		location:  location,
		imageId:   imageId,
		timeTaken: make(chan time.Time, 1),
		done:      make(chan bool, 1),
		err:       make(chan error, 1),
	}

	log.Debug().Msgf("Starting download of %s", imageId)

	// Start download in the background
	if err := startDownload(dm.ctx); err != nil {
		return nil, err
	}

	go func() {
		timeout := time.NewTimer(60 * time.Second)
		defer timeout.Stop()

		// for {
		select {
		case download := <-dm.newDownload:
			log.Debug().Msgf("Download of %s started", download.suggestedFilename)
			job.dlFile = download.filename
			job.suggestedFilename = download.suggestedFilename
			job.downloadDone = download.done
			dm.jobs <- job
			readyForNext <- true
			return
		case <-timeout.C:
			job.err <- errors.New("timeout waiting for download to start")
			readyForNext <- true
			return
		}
		// }
	}()

	return job, nil
}
