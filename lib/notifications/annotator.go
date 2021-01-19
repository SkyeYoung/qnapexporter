package notifications

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"
)

type Annotator interface {
	Post(annotation string) (int, error)
}

type grafanaAnnotation struct {
	Id      int      `json:"id,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Time    int64    `json:"time,omitempty"`
	TimeEnd int64    `json:"timeEnd,omitempty"`
	Text    string   `json:"text,omitempty"`
}

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type regionMatchingAnnotator struct {
	grafanaURL       string
	grafanaAuthToken string
	tags             []string
	cache            AnnotationCache
	client           httpClient
	logger           *log.Logger
}

func NewAnnotator(
	grafanaURL, grafanaAuthToken string,
	tags []string,
	cache AnnotationCache,
	c httpClient,
	logger *log.Logger,
) Annotator {
	return &regionMatchingAnnotator{
		grafanaURL:       grafanaURL,
		grafanaAuthToken: grafanaAuthToken,
		tags:             tags,
		cache:            cache,
		client:           c,
		logger:           logger,
	}
}

func (a *regionMatchingAnnotator) Post(annotation string) (int, error) {
	ga := grafanaAnnotation{
		Text: annotation,
		Tags: a.tags,
	}
	url := fmt.Sprintf("%s/api/annotations", a.grafanaURL)

	reqType := "POST"
	id, err := a.cache.Match(annotation)
	if err == nil && id != -1 {
		reqType = "PATCH"
		ga.TimeEnd = time.Now().UnixNano() / 1000
		url = fmt.Sprintf("%s/%d", url, id)
	}

	jsonBytes, err := json.Marshal(ga)
	if err != nil {
		a.logger.Printf("Error marshalling Grafana annotation: %v\n", err)
		return -1, err
	}
	bodyReader := bytes.NewReader(jsonBytes)
	req, err := http.NewRequest(reqType, url, bodyReader)
	if err != nil {
		a.logger.Printf("Error creating Grafana annotation request: %v\n", err)
		return -1, err
	}

	req.Header.Set("Content-Type", "application/json")
	if a.grafanaAuthToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", a.grafanaAuthToken))
	}

	resp, err := a.client.Do(req)
	if err == nil {
		if resp.StatusCode < 300 {
			body, readErr := ioutil.ReadAll(resp.Body)
			if readErr != nil {
				return -1, fmt.Errorf("reading response body: %w", readErr)
			}

			var response struct {
				Id      int    `json:"id"`
				Message string `json:"message"`
			}
			err = json.Unmarshal(body, &response)
			if err != nil {
				return -1, fmt.Errorf("unmarshaling response body: %w", err)
			}

			a.logger.Printf("%s (status: %q), ID: %d\n", response.Message, resp.Status, response.Id)
			return response.Id, nil
		}

		a.logger.Printf("Error creating Grafana annotation at %s: HTTP %d %q\n", url, resp.StatusCode, resp.Status)
		err = fmt.Errorf("call to %s failed with HTTP %d %q", url, resp.StatusCode, resp.Status)
	} else {
		a.logger.Printf("Error creating Grafana annotation at %s: %v\n", url, err)
	}

	return -1, err
}
