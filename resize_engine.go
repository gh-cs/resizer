package main

import (
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
	quit        chan struct{}     // use this channel to trap a ctrl-c and break all for {} loops, gracefully terminating the workers
	HashToImage map[string]*Image // maps hash to url and status
}

func NewResizeEngine() *ResizeEngine {
	imageInput := make(chan ImageAndOutput, 100)
	hashToImage := make(map[string]*Image)
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

			l.HashToImage[h] = &Image{
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
			l.HashToImage[h] = &Image{
				Result: imageStatusSuccess,
				URL:    fmt.Sprintf("http://localhost:8080/v1/image/%s.jpeg", h),
				Cached: true,
			}
			l.mu.Unlock()
			if iao.outputChan != nil {
				l.mu.Lock()
				iao.outputChan <- *l.HashToImage[h]
				l.mu.Unlock()
			}
			//case <-l.quit:
			//	log.Println(fmt.Sprintf("worker %s shutting down", id))
			//	break
		}
	}
}

func (l *ResizeEngine) HashImageAsync(urls []string) []*Image {
	var imageList []*Image
	for _, url := range urls {
		h := strconv.Itoa(int(hash(url)))

		l.mu.Lock()
		if _, ok := l.HashToImage[h]; ok { // hash already in map, get the object
			imageList = append(imageList, &Image{
				Result: l.HashToImage[h].Result,
				URL:    l.HashToImage[h].URL,
				Cached: l.HashToImage[h].Cached,
			})
			l.mu.Unlock() // defer not always the first choice
			continue
		}
		imageList = append(imageList, &Image{ // move this in the worker and lock it while editing
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

func (l *ResizeEngine) HashImageSync(urls []string) []*Image {
	var (
		imageList       []*Image
		imagesToProcess = 0
	)

	oc := make(chan Image, len(urls))
	for _, url := range urls {
		h := strconv.Itoa(int(hash(url)))

		l.mu.Lock()
		if _, ok := l.HashToImage[h]; ok { // hash already in map, get the object
			imageList = append(imageList, &Image{
				Result: l.HashToImage[h].Result,
				URL:    l.HashToImage[h].URL,
				Cached: l.HashToImage[h].Cached,
			})
			l.mu.Unlock()
			continue
		}
		l.mu.Unlock()

		imagesToProcess++
		l.imageInput <- ImageAndOutput{
			url:        url,
			outputChan: oc,
		}
	}

	for len(imageList) != len(urls) {
		image := <-oc
		imageList = append(imageList, &image)
	}

	return imageList
}

func (l *ResizeEngine) GetImage(hash string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// make use of early returns whenever appropriate
	if _, ok := l.HashToImage[hash]; !ok { // if the hash isn't in storage return a 404
		// we should return an error here that's cast in a meta err type with the appropriate response code
		return "", fmt.Errorf("no image found for hash %s", hash)
	}
	// return a relevant message if the status != success
	if l.HashToImage[hash].Result != imageStatusSuccess {
		return l.HashToImage[hash].Result, nil
	}
	// return the "image"
	return fmt.Sprintf("imagine this is the image for localhost/v1/image/%s.jpeg", hash), nil
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
