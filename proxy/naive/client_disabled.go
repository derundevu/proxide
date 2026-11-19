//go:build !with_naive_outbound || !((linux && !android && (386 || amd64 || arm || arm64 || loong64 || mipsle || riscv64 || (mips64le && !with_musl))) || (android && (386 || amd64 || arm || arm64)) || (darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package naive

import (
	"context"
	"fmt"

	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
)

type Client struct{}

func NewClient(context.Context, *ClientConfig) (*Client, error) {
	return nil, fmt.Errorf("naive: Chromium outbound support is unavailable; rebuild for a supported platform with with_naive_outbound (and with_purego for dynamic loading)")
}

func (*Client) Process(context.Context, *transport.Link, internet.Dialer) error {
	return fmt.Errorf("naive: Chromium outbound support is unavailable")
}

func (*Client) Close() error { return nil }
