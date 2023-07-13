package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	port := flag.Int("port", 8080, "Port to listen on")
	cadvisorFile := flag.String("cadvisorFile", "cadvisor.prom", "Path to cadvisor prometheus metrics file")
	flag.Parse()

	metrics, err := os.ReadFile(*cadvisorFile)
	if err != nil {
		log.Fatal(err)
	}

	metricData := bytes.NewReader(metrics)

	b64Decoder := base64.NewDecoder(base64.StdEncoding, metricData)
	gzReader, err := gzip.NewReader(b64Decoder)
	if err != nil {
		log.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, gzReader); err != nil {
		log.Fatal(err)
	}

	if err := gzReader.Close(); err != nil {
		log.Fatal(err)
	}

	data := buf.Bytes()

	http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")

		_, _ = w.Write(data)
	})

	log.Fatal(http.ListenAndServe(fmt.Sprintf("localhost:%d", *port), nil))
}
