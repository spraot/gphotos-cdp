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
	timeTaken         time.Time
	originalFilename  string
}

type Download struct {
	startTime         time.Time
	suggestedFilename string
	filename          string
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
		newDownload: make(chan Download, 1),
		downloads:   make(map[string]Download),
	}

	log.Debug().Msg("Starting download manager")
	chromedp.ListenTarget(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadWillBegin); ok {
			dm.downloads[ev.GUID] = Download{
				startTime:         time.Now(),
				suggestedFilename: ev.SuggestedFilename,
				filename:          ev.GUID,
				done:              make(chan error, 1),
				dlTimeout:         time.NewTimer(120 * time.Second),
			}

			log.Debug().Str("GUID", ev.GUID).Msgf("Download of %s started (EventDownloadWillBegin)", ev.SuggestedFilename)
			dm.newDownload <- dm.downloads[ev.GUID]
		}
	})

	chromedp.ListenTarget(ctx, func(v interface{}) {
		if ev, ok := v.(*browser.EventDownloadProgress); ok {
			dl := dm.downloads[ev.GUID]
			if ev.State == browser.DownloadProgressStateInProgress {
				log.Trace().Str("GUID", ev.GUID).Msgf("Download of %s progress: %.2f%%", dl.suggestedFilename, (ev.ReceivedBytes/ev.TotalBytes)*100)
				dl.dlTimeout.Reset(120 * time.Second)
			}
			if ev.State == browser.DownloadProgressStateCompleted {
				dlTime := time.Since(dl.startTime).Milliseconds()
				dlMb := ev.ReceivedBytes / 1024 / 1024
				log.Debug().Str("GUID", ev.GUID).Msgf("Download of %s completed, downloaded %.2fMB in %dms (%.2fMB/s)",
					dl.suggestedFilename,
					dlMb,
					dlTime,
					dlMb/float64(dlTime)*1000,
				)
				dl.done <- nil
				log.Trace().Str("GUID", ev.GUID).Msgf("Download of %s took %dms", dl.suggestedFilename, dlTime)
				delete(dm.downloads, ev.GUID)
				log.Trace().Str("GUID", ev.GUID).Msgf("Download of %s removed from download list", dl.suggestedFilename)
			}
			if ev.State == browser.DownloadProgressStateCanceled {
				log.Debug().Str("GUID", ev.GUID).Msgf("Download of %s cancelled", dl.suggestedFilename)
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
		<-job.downloadDone
		log.Debug().Msgf("Download took %dms", time.Since(job.st).Milliseconds())

		err := dm.session.dlAndMove(job)
		if err != nil {
			job.err <- err
			continue
		}

		// Update file dates before moving
		if err := dm.session.doFileDateUpdate(job); err != nil {
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

func (dm *DownloadManager) StartJob(location, imageId string, readyForNext chan bool) *DownloadJob {
	job := &DownloadJob{
		st:           time.Now(),
		location:     location,
		imageId:      imageId,
		done:         make(chan bool, 1),
		err:          make(chan error, 1),
		downloadDone: make(chan bool, 1),
	}

	go func() {
		dlStartTimeout1 := time.NewTimer(25 * time.Second)
		defer dlStartTimeout1.Stop()
		dlStartTimeout2 := time.NewTimer(40 * time.Second)
		defer dlStartTimeout2.Stop()

		log.Debug().Msgf("Starting download of %s", imageId)

		// Start download in the background
		if err := requestDownload2(dm.ctx); err != nil {
			job.err <- err
			return
		}

		// Get photo data in the background
		donePhotoDataRead := make(chan bool, 1)
		go func() {
			if *fileDateFlag {
				var err error
				job.timeTaken, job.originalFilename, err = dm.session.getPhotoData(dm.ctx, imageId)
				if err != nil {
					job.err <- err
					donePhotoDataRead <- true
					return
				}
			}
			donePhotoDataRead <- true
		}()

		log.Trace().Msg("Waiting for download to start")
		for {
			select {
			case download := <-dm.newDownload:
				log.Debug().Msgf("Download of %s (%s) started in %dms", job.imageId, download.suggestedFilename, time.Since(job.st).Milliseconds())
				job.dlFile = download.filename
				job.suggestedFilename = download.suggestedFilename

				go func() {
					select {
					case err := <-download.done:
						download.dlTimeout.Stop()
						if err != nil {
							job.err <- err
						} else {
							job.downloadDone <- true
						}
					case <-download.dlTimeout.C:
						job.err <- errors.New("timeout waiting for download progress")
					}
				}()

				select {
				case <-donePhotoDataRead:
					dm.jobs <- job
				case err := <-job.err:
					job.err <- err
				}
				readyForNext <- true
				return
			case <-dlStartTimeout1.C:
				log.Info().Msgf("Timeout waiting for download to start, trying again")
				if err := requestDownload1(dm.ctx); err != nil {
					job.err <- err
					readyForNext <- true
					return
				}
				log.Debug().Msg("Tried again, waiting for download to start")
			case <-dlStartTimeout2.C:
				job.err <- errors.New("timeout waiting for download to start")
				readyForNext <- true
				return
			}
		}
	}()

	return job
}
