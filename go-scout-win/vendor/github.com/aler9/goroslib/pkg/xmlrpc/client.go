// Package xmlrpc implements the XML-RPC protocol, in a variant fit for ROS.
package xmlrpc

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	clientTimeout = 10 * time.Second
)

var httpClient = &http.Client{
	Timeout: clientTimeout,
}

// Client is a XML-RPC client.
type Client struct {
	url string
}

// NewClient allocates a Client.
func NewClient(host string) *Client {
	// Monkey-patch fix for Moorebot Scout:
	// If the host is "linaro-alip" and we have a target IP set, use it.
	if strings.Contains(host, "linaro-alip") {
		targetIP := os.Getenv("SCOUT_TARGET_IP")
		if targetIP != "" {
			host = strings.Replace(host, "linaro-alip", targetIP, 1)
		}
	}

	return &Client{
		url: (&url.URL{
			Scheme: "http",
			Host:   host,
			Path:   "/RPC2",
		}).String(),
	}
}

// Do writes a request and reads a response.
func (c *Client) Do(method string, paramsReq interface{}, paramsRes interface{}) error {
	var buf bytes.Buffer
	err := requestEncode(&buf, method, paramsReq)
	if err != nil {
		return err
	}

	res, err := httpClient.Post(c.url, "text/xml", &buf)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		io.Copy(io.Discard, res.Body)
		return fmt.Errorf("bad status code: %d", res.StatusCode)
	}

	err = responseDecode(res.Body, paramsRes)
	if err != nil {
		return err
	}

	return nil
}
