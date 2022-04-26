package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

func main() {
	engine := NewResizeEngine()

	r := mux.NewRouter()
	// I've been using closures here for the following reasons:
	// 1. to flex
	// 2. to not create a controller
	// I would use a controller as a jumping point to the actual business logic and from there to persistence
	// also to make sure the payload/url params/variables are correct and populated accordingly

	// normally i'd make subrouters for the api and any version of it
	r.HandleFunc("/v1/resize",
		func(w http.ResponseWriter, r *http.Request) { // using closures here just to flex
			// { "urls": ["a","b","c"] }
			// BIG ASSUMPTION here that the payload is *correct*
			// in a production environment we would validate the payload via the playground validator
			// also we'd make sure all fields are populated
			// normally we would have a json friendly error type with relevant info i.e. what part of the payload failed
			// or if the error was internal or of a 3rd party i.e. imgur is down
			// these are usually detailed in a spec of the architect's choice i.e. swagger or openapi
			var async bool
			val, ok := r.URL.Query()["async"]
			if !ok {
				async = false
			} else {
				async, _ = strconv.ParseBool(val[0])
			}

			type PL struct {
				URLs []string `json:"urls"`
			}
			var payload PL
			// the part below is very slimmed down
			// ideally we would enforce a maxbytesreader on the request body and disallow unknown fields
			body, _ := io.ReadAll(r.Body)
			dec := json.NewDecoder(bytes.NewReader(body))

			_ = dec.Decode(&payload)

			// return err for duplicates OR remove duplicates directly, whatever works
			uniqueURLs := removeDuplicateStr(payload.URLs)
			// switch cases for async here and point to different looper functions
			var res []byte
			switch async {
			case true:
				res, _ = json.Marshal(engine.HashImageAsync(uniqueURLs))
			case false:
				// the trick here is we want to concurrently process images even when
				// async is false but wait for all the results before displaying them
				// this is preferred from a performance point of view as opposed to iterating through the url slice
				// create an output channel per request and receive the set of processed images on that channel only
				res, _ = json.Marshal(engine.HashImageSync(uniqueURLs))
			}

			_, _ = w.Write(res)
		},
	).Methods(http.MethodPost)
	r.HandleFunc("/v1/image/{image_hash}.jpeg",
		func(w http.ResponseWriter, r *http.Request) {
			// again, horribly slimmed down
			// these should be checked
			_, cancel := context.WithTimeout(r.Context(), 6*time.Second)
			defer cancel()

			hash := mux.Vars(r)["image_hash"]
			res, err := engine.GetImage(hash)
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(err.Error()))
				return
			}

			_, _ = w.Write([]byte(res)) // a considerable amount of unchecked errors
		},
	).Methods(http.MethodGet)

	go engine.Loop()

	log.Fatal(http.ListenAndServe(":8080", r))
}

func removeDuplicateStr(strSlice []string) []string {
	allKeys := make(map[string]bool)
	var list []string
	for _, item := range strSlice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}
