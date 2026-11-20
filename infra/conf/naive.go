package conf

import (
	"github.com/xtls/xray-core/proxy/naive"
	"google.golang.org/protobuf/proto"
)

type NaiveClientConfig struct {
	Address               *Address          `json:"address"`
	Port                  uint16            `json:"port"`
	Username              string            `json:"user"`
	Password              string            `json:"pass"`
	Level                 uint32            `json:"level"`
	QUIC                  bool              `json:"quic"`
	QUICCongestionControl string            `json:"quicCongestionControl"`
	Headers               map[string]string `json:"headers"`
	UDPOverTCP            bool              `json:"udpOverTcp"`
}

func (c *NaiveClientConfig) Build() (proto.Message, error) {
	config := &naive.ClientConfig{
		Port:                  uint32(c.Port),
		Username:              c.Username,
		Password:              c.Password,
		Level:                 c.Level,
		Quic:                  c.QUIC,
		QuicCongestionControl: c.QUICCongestionControl,
		Headers:               c.Headers,
		UdpOverTcp:            c.UDPOverTCP,
	}
	if c.Address != nil && c.Address.Address != nil {
		config.Address = c.Address.Build()
	}
	if err := naive.ValidateClientConfig(config); err != nil {
		return nil, err
	}
	return config, nil
}
