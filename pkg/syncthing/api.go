package syncthing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/syncthing/syncthing/lib/config"

	config2 "github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type Client struct {
	GUIAddress string
	client     *http.Client
}

func NewClient(addr string) *Client {
	c := &Client{
		GUIAddress: addr,
		client:     &http.Client{Timeout: 5 * time.Second},
	}
	return c
}

func (c *Client) GetConfig(ctx context.Context) (*config.Configuration, error) {
	buf, err := c.Call(ctx, "rest/config", "GET", nil)
	var configuration config.Configuration
	err = json.Unmarshal(buf, &configuration)
	if err != nil {
		return nil, err
	}
	return &configuration, nil
}

func (c *Client) PutConfig(ctx context.Context, conf *config.Configuration) error {
	marshal, err := json.Marshal(conf)
	if err != nil {
		return err
	}
	_, err = c.Call(ctx, "rest/config", "PUT", marshal)
	return err
}

func (c *Client) Call(ctx context.Context, uri, method string, body []byte) ([]byte, error) {
	var url = path.Join(c.GUIAddress, uri)
	req, err := http.NewRequest(method, fmt.Sprintf("http://%s", url), bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize syncthing API request: %w", err)
	}
	req = req.WithContext(ctx)
	req.Header.Set("X-API-Key", config2.SyncthingAPIKey)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call syncthing [%s]: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected response from syncthing [%s | %d]: %s", req.URL.String(), resp.StatusCode, string(body))
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from syncthing [%s]: %w", uri, err)
	}
	return body, nil
}
