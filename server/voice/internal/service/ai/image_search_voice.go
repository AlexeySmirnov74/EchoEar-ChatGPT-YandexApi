package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const imageSearchSocket = "/data/image-experiment-search/search.sock"
const imageFrameBytes = 240 * 240 * 2
var errImageNoResults = errors.New("image search returned no results")

type imageSearchResult struct { sequence uint64; pixels []byte; err error }
type imageSearchFetcherKey struct{}
type imageSearchFetcher func(context.Context, string) ([]byte, error)

func imageRealtimeTool() map[string]any {
	return map[string]any{"type":"function", "name":"show_image", "description":"Search for an existing image, photograph or drawing and send it to the speaker. Use when asked to show, draw, create or generate a picture; this searches existing images and does not draw or generate one. Give a concise query matching the current request in its language; for a drawing request search for an existing drawing. Search is asynchronous: say you are searching for an existing image; never claim to have drawn, generated, found, displayed, seen or inspected it without confirmation.", "parameters":map[string]any{"type":"object", "properties":map[string]any{"query":map[string]any{"type":"string", "description":"Image search query"}}, "required":[]string{"query"}, "additionalProperties":false}}
}

func imageSearchQuery(args map[string]any) (string, bool) {
	if len(args) != 1 { return "", false }
	query, ok := args["query"].(string)
	query = strings.TrimSpace(query)
	if !ok || !utf8.ValidString(query) || query == "" || utf8.RuneCountInString(query) > 200 || len(query) > 800 { return "", false }
	for _, r := range query { if unicode.IsControl(r) { return "", false } }
	return query, true
}

func fetchImageSearch(ctx context.Context, query string) ([]byte,error) {
	return fetchImageSearchAt(ctx, imageSearchSocket, query)
}

// Connect only to a local Unix socket; no proxy, DNS, redirects or credentials.
func fetchImageSearchAt(ctx context.Context, socket, query string) ([]byte,error) {
	if _, valid := imageSearchQuery(map[string]any{"query":query}); !valid { return nil,errors.New("invalid image query") }
	body, err := json.Marshal(map[string]string{"query":query}); if err != nil { return nil,err }
	dialer := &net.Dialer{Timeout:2*time.Second}
	transport := &http.Transport{Proxy:nil, DialContext:func(ctx context.Context,_,_ string)(net.Conn,error){ return dialer.DialContext(ctx,"unix",socket) }, DisableKeepAlives:true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport:transport, CheckRedirect:func(*http.Request,[]*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://image.local/search", bytes.NewReader(body)); if err != nil { return nil,err }
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req); if err != nil { return nil,errors.New("image service unavailable") }
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {return nil,errImageNoResults}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/octet-stream" { return nil,errors.New("image service rejected request") }
	data, err := io.ReadAll(io.LimitReader(resp.Body, imageFrameBytes+1))
	if err != nil || len(data) != imageFrameBytes { return nil,errors.New("invalid image frame") }
	return data,nil
}
