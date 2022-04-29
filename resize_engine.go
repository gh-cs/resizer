package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	imageStatusSuccess    = "success"
	imageStatusInProgress = "in progress"
)

type Image struct {
	Result string `json:"result"`
	URL    string `json:"url"`
	Cached bool   `json:"cached"`
}

type ImageAndOutput struct {
	url        string
	outputChan chan Image
}

type ResizeEngine struct {
	mu          sync.Mutex
	imageInput  chan ImageAndOutput
	quit        chan struct{}    // use this channel to trap a ctrl-c and break all for {} loops, gracefully terminating the workers
	HashToImage map[string]Image // maps hash to url and status
}

func NewResizeEngine() *ResizeEngine {
	imageInput := make(chan ImageAndOutput, 100)
	hashToImage := make(map[string]Image)
	quit := make(chan struct{}, 100)
	return &ResizeEngine{
		imageInput:  imageInput,
		quit:        quit,
		HashToImage: hashToImage,
	}
}

func (l *ResizeEngine) Loop() {
	var wg sync.WaitGroup
	// waitgroups are the cheapest and most straightforward answer to worker pools
	// there's also another answer to this which is more esoteric
	// as always - the right tool for the right job
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		id := uuid.New()
		go func() {
			defer wg.Done()
			l.worker(id.String())
		}()
	}
}

func (l *ResizeEngine) worker(id string) {
	log.Printf("worker %s started\n", id)
	for {
		select {
		case iao := <-l.imageInput:
			log.Printf("worker %s got a job for %s\n", id, iao.url)

			h := strconv.Itoa(int(hash(iao.url)))
			// check if map has hash
			l.mu.Lock()
			if _, ok := l.HashToImage[h]; ok {
				// if the hash is already in the map return the object on the channel (if the channel exists)
				if iao.outputChan != nil {
					iao.outputChan <- Image{
						Result: l.HashToImage[h].Result,
						URL:    l.HashToImage[h].URL,
						Cached: l.HashToImage[h].Cached,
					}
				}
				l.mu.Unlock()
				continue
			}

			l.HashToImage[h] = Image{
				Result: imageStatusInProgress,
				URL:    fmt.Sprintf("http://localhost:8080/v1/image/%s.jpeg", h),
				Cached: false,
			}
			l.mu.Unlock()

			time.Sleep(100 * time.Millisecond)
			log.Println("fetching ", iao.url)
			time.Sleep(time.Duration(rand.Intn(3)+5) * time.Second)
			log.Println("fetched ", iao.url)
			log.Println("resizing ", iao.url)
			time.Sleep(time.Duration(rand.Intn(3)+5) * time.Second)
			log.Println(iao.url, " resized")

			l.mu.Lock()
			l.HashToImage[h] = Image{
				Result: imageStatusSuccess,
				URL:    fmt.Sprintf("http://localhost:8080/v1/image/%s.jpeg", h),
				Cached: true,
			}
			l.mu.Unlock()
			if iao.outputChan != nil {
				l.mu.Lock()
				iao.outputChan <- l.HashToImage[h]
				l.mu.Unlock()
			}
		}
	}
}

func (l *ResizeEngine) HashImageAsync(urls []string) []Image {
	var imageList []Image
	for _, url := range urls {
		h := strconv.Itoa(int(hash(url)))

		l.mu.Lock()
		if _, ok := l.HashToImage[h]; ok { // hash already in map, get the object
			imageList = append(imageList, Image{
				Result: l.HashToImage[h].Result,
				URL:    l.HashToImage[h].URL,
				Cached: l.HashToImage[h].Cached,
			})
			l.mu.Unlock() // defer not always the first choice
			continue
		}
		imageList = append(imageList, Image{ // move this in the worker and lock it while editing
			Result: imageStatusInProgress,
			URL:    fmt.Sprintf("http://localhost:8080/v1/image/%s.jpeg", h),
			Cached: false,
		})
		l.mu.Unlock()

		l.imageInput <- ImageAndOutput{
			url:        url,
			outputChan: nil,
		}
	}
	return imageList
}

func (l *ResizeEngine) HashImageSync(urls []string) []Image {
	var imageList []Image

	oc := make(chan Image, len(urls))
	for _, url := range urls {
		h := strconv.Itoa(int(hash(url)))

		l.mu.Lock()
		if _, ok := l.HashToImage[h]; ok { // hash already in map, get the object
			// check if the found image is in status success
			// otherwise wait for it
			imageList = append(imageList, Image{
				Result: l.HashToImage[h].Result,
				URL:    l.HashToImage[h].URL,
				Cached: l.HashToImage[h].Cached,
			})
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()

		l.imageInput <- ImageAndOutput{
			url:        url,
			outputChan: oc,
		}
	}

	for len(imageList) != len(urls) {
		image := <-oc
		imageList = append(imageList, image)
	}

	return imageList
}

func (l *ResizeEngine) HashImageSyncV2(ctx context.Context, urls []string) ([]Image, error) {
	var (
		imageList       []Image
		imagesToWaitFor []string
	)
	t := time.NewTicker(1 * time.Second)

	oc := make(chan Image, len(urls))
	for _, url := range urls {
		h := strconv.Itoa(int(hash(url)))

		l.mu.Lock()
		i, ok := l.HashToImage[h]
		l.mu.Unlock()

		if !ok { // send the image for processing if the hash isn't in storage
			l.imageInput <- ImageAndOutput{
				url:        url,
				outputChan: oc,
			}
		}

		// best case scenario, grab the image and add it to the images slice
		if i.Result == imageStatusSuccess {
			imageList = append(imageList, Image{
				Result: l.HashToImage[h].Result,
				URL:    l.HashToImage[h].URL,
				Cached: l.HashToImage[h].Cached,
			})
		}

		// anything else is in processing
		imagesToWaitFor = append(imagesToWaitFor, h)
	}

	// very happy scenario, all images are done already, don't wait for any ticks
	if len(urls) == len(imageList) {
		return imageList, nil
	}

	for {
		select {
		case <-t.C:
			// check the map for all hashes with status != "success"
			for _, wh := range imagesToWaitFor {
				l.mu.Lock()
				i, _ := l.HashToImage[wh]
				l.mu.Unlock()
				if i.Result == imageStatusSuccess {
					// an improvement here would be to resize the imagesToWaitFor slice without this particular element
					imageList = append(imageList, i)
					if len(imageList) == len(urls) {
						return imageList, nil
					}
				}
			}
		case image := <-oc:
			// one of the images we sent for processing came back
			// we add it to the results
			imageList = append(imageList, image)
			if len(imageList) == len(urls) {
				return imageList, nil
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("operation timed out: %w", ctx.Err())
		}
	}
}

func (l *ResizeEngine) GetImage(ctx context.Context, hash string) (string, error) {
	// use a ticker because we don't to bombard the map with lock/unlock commands
	// the max number of ticks is effectively capped at the passed context's timeout duration
	t := time.NewTicker(1 * time.Second)

	l.mu.Lock()
	img, ok := l.HashToImage[hash]
	l.mu.Unlock()

	// make use of early returns whenever appropriate
	if !ok { // if the hash isn't in storage return a 404
		// we should return an error here that's cast in a meta err type with the appropriate response code
		return "", fmt.Errorf("no image found for hash %s", hash)
	}

	// have this check here in case the image is already resized and stored so that we don't wait a tick
	if img.Result == imageStatusSuccess {
		return fmt.Sprintf("imagine this is the image for localhost/v1/image/%s.jpeg", hash), nil
	}

	// by this point we are sure the image is processing i.e. the status is != "success"
	// so we wait until the status == "success" for X number of ticks
	// or until the context timeout runs out, whichever comes first
	for {
		select {
		case <-t.C:
			l.mu.Lock()
			img, _ := l.HashToImage[hash]
			l.mu.Unlock()
			if img.Result == imageStatusSuccess {
				return fmt.Sprintf("imagine this is the image for localhost/v1/image/%s.jpeg", hash), nil
			}
		case <-ctx.Done():
			// ideally this sort of error should be its own type and encapsulate ctx.Err()
			// it has to return a 408 status: http.StatusRequestTimeout
			return "", fmt.Errorf("operation timed out: %w", ctx.Err())
		}
	}
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
