package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Bucket struct {
	ID          string          `json:"id"`
	Created     time.Time       `json:"created"`
	Name        *string         `json:"name"`
	Type        string          `json:"type"`
	Client      string          `json:"client"`
	Hostname    string          `json:"hostname"`
	Data        json.RawMessage `json:"data"`
	LastUpdated time.Time       `json:"last_updated"`
}
type Buckets map[string]Bucket

type Event struct {
	ID        int             `json:"id"`
	Timestamp time.Time       `json:"timestamp"`
	Duration  float64         `json:"duration"`
	Data      json.RawMessage `json:"data"`
}

type WebTabCurrent struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Audible   bool   `json:"audible"`
	Incognito bool   `json:"incognito"`
}

type AppEditorActivity struct {
	File     string `json:"file"`
	Project  string `json:"project"`
	Language string `json:"language"`
}

type CurrentWindow struct {
	App   string `json:"app"`
	Title string `json:"title"`
}

type AfkStatus struct {
	Status string `json:"status"`
}

type StopWatch struct {
	Label   string `json:"label"`
	Running bool   `json:"running"`
}

type Config struct {
	Bucket           string `json:"Bucket"`
	InfluxDBHost     string `json:"InfluxDBHost"`
	InfluxDBApiToken string `json:"InfluxDBApiToken"`
	Org              string `json:"Org"`
	ActivityWatchUrl string `json:"ActivityWatchUrl"`
}

type retryableTransport struct {
	transport             http.RoundTripper
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
}

const bucketsApiPath = "/api/0/buckets"
const webTabCurrentType = "web.tab.current"
const appEditorType = "app.editor.activity"
const currentWindowType = "currentwindow"
const stopwatchType = "general.stopwatch"
const afkType = "afkstatus"
const retryCount = 3
const stringLimit = 1024

func shouldRetry(err error, resp *http.Response) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (t *retryableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}
	resp, err := t.transport.RoundTrip(req)
	retries := 0
	for shouldRetry(err, resp) && retries < retryCount {
		backoff := time.Duration(math.Pow(2, float64(retries))) * time.Second
		time.Sleep(backoff)
		if resp != nil && resp.Body != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if req.Body != nil {
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}
		if resp != nil && resp.Status != "" {
			log.Printf("Previous request failed with %s", resp.Status)
		}
		log.Printf("Retry %d of request to: %s", retries+1, req.URL)
		resp, err = t.transport.RoundTrip(req)
		retries++
	}
	return resp, err
}

func handleApiError(message string, err error, apiErrors *atomic.Int64) {
	apiErrors.Add(1)
	log.SetOutput(os.Stderr)
	log.Println(message, err)
	log.SetOutput(os.Stdout)
}

func escapeTagValue(value string) string {
	withoutCommas := strings.ReplaceAll(value, ",", `\,`)
	withoutEquals := strings.ReplaceAll(withoutCommas, "=", `\=`)
	escaped := strings.ReplaceAll(withoutEquals, ` `, `\ `)
	runes := []rune(escaped)
	if len(runes) <= stringLimit {
		return escaped
	}
	return string(runes[0:stringLimit-3]) + "..."
}

func main() {
	confFilePath := "activitywatch_exporter.json"
	confData, err := os.Open(confFilePath)
	if err != nil {
		log.Fatalln("Error reading config file: ", err)
	}
	defer confData.Close()
	var config Config
	err = json.NewDecoder(confData).Decode(&config)
	if err != nil {
		log.Fatalln("Error reading configuration: ", err)
	}
	if config.ActivityWatchUrl == "" {
		log.Fatalln("ActivityWatchUrl is required")
	}
	if config.Bucket == "" {
		log.Fatalln("Bucket is required")
	}
	if config.InfluxDBHost == "" {
		log.Fatalln("InfluxDBHost is required")
	}
	if config.InfluxDBApiToken == "" {
		log.Fatalln("InfluxDBApiToken is required")
	}
	if config.Org == "" {
		log.Fatalln("Org is required")
	}

	var days int
	flag.IntVar(&days, "days", 1, "Number of days in the past to fetch")
	flag.Parse()

	transport := &retryableTransport{
		transport:             &http.Transport{},
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	var apiErrors atomic.Int64
	bucketsReq, _ := http.NewRequest("GET", config.ActivityWatchUrl+bucketsApiPath, nil)
	bucketsResp, err := client.Do(bucketsReq)
	if err != nil {
		log.Fatalln("Error trying to get bucket list: ", err)
	}
	defer bucketsResp.Body.Close()
	bucketsBody, err := io.ReadAll(bucketsResp.Body)
	if err != nil {
		log.Fatalln("Error reading bucket list data: ", err)
	}
	if bucketsResp.StatusCode != http.StatusOK {
		log.Fatalf("Error trying to get bucket list: %s\n", string(bucketsBody))
	}

	var bucketsList Buckets
	err = json.Unmarshal(bucketsBody, &bucketsList)
	if err != nil {
		log.Fatalln("Error unmarshalling bucket list data: ", err)
	}

	wg := &sync.WaitGroup{}
	payload := bytes.Buffer{}
	for _, entry := range bucketsList {
		wg.Add(1)

		go func(payload *bytes.Buffer, apiErrors *atomic.Int64) {
			defer wg.Done()

			start := time.Now().AddDate(0, 0, -days).Format("2006-01-02T15:04:05.000000-07:00")
			eventsUrl := fmt.Sprintf(config.ActivityWatchUrl+bucketsApiPath+"/%s/events?start=%s", entry.ID, url.QueryEscape(start))
			eventsReq, _ := http.NewRequest("GET", eventsUrl, nil)
			eventsResp, err := client.Do(eventsReq)
			if err != nil {
				handleApiError(fmt.Sprintf("Error trying to get events for bucket=%s: ", entry.ID), err, apiErrors)
				return
			}
			defer eventsResp.Body.Close()
			eventsBody, err := io.ReadAll(eventsResp.Body)
			if err != nil {
				handleApiError(fmt.Sprintf("Error reading events data for bucket=%s: ", entry.ID), err, apiErrors)
				return
			}
			if eventsResp.StatusCode != http.StatusOK {
				handleApiError(fmt.Sprintf("Error trying to get events data for bucket=%s:\n", entry.ID), err, apiErrors)
				return
			}
			var events []Event
			err = json.Unmarshal(eventsBody, &events)
			if err != nil {
				handleApiError(fmt.Sprintf("Error unmarshalling events data for bucket=%s api response data: %s", entry.ID, string(eventsBody)), err, apiErrors)
				return
			}

			for _, event := range events {
				var influxLine string
				switch entry.Type {
				case webTabCurrentType:
					data := new(WebTabCurrent)
					err := json.Unmarshal(event.Data, data)
					if err != nil {
						log.Printf("Error unmarshalling event data for bucket=%s data=%s: %s\n", entry.ID, event.Data, err)
						continue
					}
					u, err := url.Parse(data.URL)
					if err != nil {
						log.Printf("Error parsing URL=%s: %s\n", data.URL, err)
						continue
					}
					var cleanUrl string
					if u.Host == "" {
						cleanUrl = ""

					} else {
						cleanUrl = fmt.Sprintf(",url=%s", u.Host)
					}
					influxLine = fmt.Sprintf("%s,client=%s,hostname=%s%s duration=%.3f,audible=%t,incognito=%t %v\n",
						entry.Type,
						entry.Client,
						escapeTagValue(entry.Hostname),
						cleanUrl,
						event.Duration,
						data.Audible,
						data.Incognito,
						event.Timestamp.Unix(),
					)
				case appEditorType:
					data := new(AppEditorActivity)
					err := json.Unmarshal(event.Data, data)
					if err != nil {
						log.Printf("Error unmarshalling event data for bucket=%s data=%s: %s\n", entry.ID, event.Data, err)
						continue
					}
					influxLine = fmt.Sprintf("%s,client=%s,hostname=%s,project=%s,language=%s,file=%s duration=%.3f %v\n",
						entry.Type,
						entry.Client,
						escapeTagValue(entry.Hostname),
						escapeTagValue(data.Project),
						escapeTagValue(data.Language),
						escapeTagValue(data.File),
						event.Duration,
						event.Timestamp.Unix(),
					)
				case currentWindowType:
					data := new(CurrentWindow)
					err := json.Unmarshal(event.Data, data)
					if err != nil {
						log.Printf("Error unmarshalling event data for bucket=%s data=%s: %s\n", entry.ID, event.Data, err)
						continue
					}
					influxLine = fmt.Sprintf("%s,client=%s,hostname=%s,app=%s duration=%.3f %v\n",
						entry.Type,
						entry.Client,
						escapeTagValue(entry.Hostname),
						escapeTagValue(data.App),
						event.Duration,
						event.Timestamp.Unix(),
					)
				case stopwatchType:
					data := new(StopWatch)
					err := json.Unmarshal(event.Data, data)
					if err != nil {
						log.Printf("Error unmarshalling event data for bucket=%s data=%s: %s\n", entry.ID, event.Data, err)
						continue
					}
					var label string
					if data.Label == "" {
						label = ""

					} else {
						label = fmt.Sprintf(",label=%s", escapeTagValue(data.Label))
					}
					influxLine = fmt.Sprintf("%s,client=%s,hostname=%s%s duration=%.3f,running=%t %v\n",
						entry.Type,
						entry.Client,
						escapeTagValue(entry.Hostname),
						label,
						event.Duration,
						data.Running,
						event.Timestamp.Unix(),
					)
				case afkType:
					data := new(AfkStatus)
					err := json.Unmarshal(event.Data, data)
					if err != nil {
						log.Printf("Error unmarshalling event data for bucket=%s data=%s: %s\n", entry.ID, event.Data, err)
						continue
					}
					influxLine = fmt.Sprintf("%s,client=%s,hostname=%s duration=%.3f,status=\"%s\" %v\n",
						entry.Type,
						entry.Client,
						escapeTagValue(entry.Hostname),
						event.Duration,
						data.Status,
						event.Timestamp.Unix(),
					)
				default:
					log.Printf("Skipping unknown event type: %s\n", entry.Type)
					continue
				}

				payload.WriteString(influxLine)
			}

		}(&payload, &apiErrors)

	}

	wg.Wait()

	if len(payload.Bytes()) == 0 {
		log.Fatalln("No data to send")
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(payload.Bytes())
	err = w.Close()
	if err != nil {
		log.Fatalln("Error compressing data: ", err)
	}
	url := fmt.Sprintf("https://%s/api/v2/write?precision=s&org=%s&bucket=%s", config.InfluxDBHost, config.Org, config.Bucket)
	post, _ := http.NewRequest("POST", url, &buf)
	post.Header.Set("Accept", "application/json")
	post.Header.Set("Authorization", "Token "+config.InfluxDBApiToken)
	post.Header.Set("Content-Encoding", "gzip")
	post.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := client.Do(post)
	if err != nil {
		log.Fatalln("Error sending data: ", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalln("Error reading data: ", err)
	}
	if resp.StatusCode != 204 {
		log.Fatal("Error sending data: ", string(body))
	}

	if apiErrors.Load() > 0 {
		log.Fatalf("Errors: %d\n", apiErrors.Load())
	}
}
