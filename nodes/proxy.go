package nodes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"go.uber.org/zap"
)

type (
	// ProxyData is a JSON-serializable type for proxy responses.
	ProxyData []byte

	// A ProxyResponse is the response for a proxied API request from a node.
	ProxyResponse struct {
		NodeID     NodeID    `json:"nodeID"`
		StatusCode int       `json:"statusCode"`
		Error      error     `json:"-"` // errors are not JSON-serializable
		Data       ProxyData `json:"data"`
	}
)

// MarshalJSON implements the json.Marshaler interface.
func (pd ProxyData) MarshalJSON() ([]byte, error) {
	str := strings.Trim(strings.TrimSpace(string(pd)), `"`)
	if strings.HasPrefix(str, "{") && strings.HasSuffix(str, "}") || strings.HasPrefix(str, "[") && strings.HasSuffix(str, "]") {
		// likely a JSON object, return as-is
		return []byte(str), nil
	}
	// treat as a string
	return []byte(`"` + str + `"`), nil
}

func (m *Manager) ProxyRequest(ctx context.Context, filter, httpMethod, path string, body io.Reader) ([]ProxyResponse, error) {
	log := m.log.Named("proxy").With(zap.String("filter", filter), zap.String("method", httpMethod), zap.String("path", path))

	active := m.Nodes()
	requestReaders := make([]io.ReadCloser, 0, len(active))
	requestWriters := make([]*io.PipeWriter, 0, len(active))
	writers := make([]io.Writer, 0, len(active))
	backends := active[:0]
	for _, node := range active {
		if node.Type == NodeType(filter) || node.ID.String() == filter {
			r, w := io.Pipe()
			requestReaders = append(requestReaders, r)
			requestWriters = append(requestWriters, w)
			writers = append(writers, w)
			backends = append(backends, node)
			log.Debug("matched node", zap.Stringer("node", node.ID), zap.String("type", string(node.Type)), zap.String("apiURL", node.APIAddress))
		}
	}

	// pipe the request body to all backends
	mw := io.MultiWriter(writers...)
	tr := io.TeeReader(body, mw)

	go func() {
		io.Copy(io.Discard, tr)
		for _, rw := range requestWriters {
			rw.Close()
		}
	}()

	var wg sync.WaitGroup
	responseCh := make(chan ProxyResponse, 1)

	for i, node := range backends {
		u, err := url.Parse(fmt.Sprintf("%s%s", node.APIAddress, path))
		if err != nil {
			return nil, fmt.Errorf("failed to parse URL: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, httpMethod, u.String(), requestReaders[i])
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.SetBasicAuth("", node.Password)
		log.Debug("proxying request", zap.String("node", node.ID.String()), zap.Stringer("url", req.URL))

		wg.Add(1)
		go func(node Node, req *http.Request) {
			defer wg.Done()

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Debug("failed to send request", zap.Error(err))
				responseCh <- ProxyResponse{NodeID: node.ID, Error: err}
				return
			}
			defer resp.Body.Close()

			data, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Debug("failed to read response body", zap.Error(err))
				responseCh <- ProxyResponse{NodeID: node.ID, Error: err}
				return
			}
			log.Debug("received response", zap.String("status", resp.Status), zap.Int("size", len(data)))
			responseCh <- ProxyResponse{NodeID: node.ID, StatusCode: resp.StatusCode, Data: data}
		}(node, req)
	}

	go func() {
		wg.Wait()
		close(responseCh)
	}()

	var responses []ProxyResponse
	for resp := range responseCh {
		log.Debug("adding response", zap.Stringer("node", resp.NodeID), zap.Int("status", resp.StatusCode), zap.Int("size", len(resp.Data)))
		responses = append(responses, resp)
	}
	return responses, nil
}
