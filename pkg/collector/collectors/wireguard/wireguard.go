package wireguard

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JulienBalestra/monitoring/pkg/collector"
	"github.com/JulienBalestra/monitoring/pkg/metrics"
	"github.com/JulienBalestra/monitoring/pkg/tagger"
	stun "github.com/JulienBalestra/wireguard-stun/pkg/wireguard"
	"golang.zx2c4.com/wireguard/wgctrl"
)

const (
	CollectorName = "wireguard"

	wireguardMetricPrefix = "wireguard."

	none = "none"
)

type Collector struct {
	conf     *collector.Config
	measures *metrics.Measures
}

func NewWireguard(conf *collector.Config) collector.Collector {
	return collector.WithDefaults(&Collector{
		conf:     conf,
		measures: metrics.NewMeasures(conf.MetricsClient.ChanSeries),
	})
}

func (c *Collector) SubmittedSeries() float64 {
	return c.measures.GetTotalSubmittedSeries()
}

func (c *Collector) DefaultTags() []string {
	return []string{
		"collector:" + CollectorName,
	}
}

func (c *Collector) Tags() []string {
	return append(c.conf.Tagger.GetUnstable(c.conf.Host), c.conf.Tags...)
}

func (c *Collector) DefaultOptions() map[string]string {
	return map[string]string{}
}

func (c *Collector) DefaultCollectInterval() time.Duration {
	return time.Second * 10
}

func (c *Collector) Config() *collector.Config {
	return c.conf
}

func (c *Collector) IsDaemon() bool { return false }

func (c *Collector) Name() string {
	return CollectorName
}

func getAllowedIPsTag(n []net.IPNet) string {
	if len(n) == 0 {
		return none
	}
	if len(n) == 1 {
		return n[0].String()
	}
	var allowedIps []string
	for _, i := range n {
		s := i.String()
		allowedIps = append(allowedIps, s)
	}
	sort.Strings(allowedIps)
	return strings.Join(allowedIps, ",")
}

func (c *Collector) Collect(_ context.Context) error {
	wgc, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer wgc.Close()

	devices, err := wgc.Devices()
	if err != nil {
		return err
	}

	now := time.Now()
	hostTags := c.Tags()
	for _, device := range devices {
		deviceTag := tagger.NewTagUnsafe("device", device.Name)
		for _, peer := range device.Peers {
			if peer.Endpoint == nil {
				continue
			}
			peer := stun.NewPeer(&peer)
			c.conf.Tagger.Update(peer.PublicKey.String(),
				tagger.NewTagUnsafe("pub-key-sha1", peer.PublicKeyHash),
				tagger.NewTagUnsafe("pub-key-sha1-7", peer.PublicKeyHash[:7]),
				tagger.NewTagUnsafe("allowed-ips", getAllowedIPsTag(peer.AllowedIPs)),
				tagger.NewTagUnsafe("endpoint", peer.Endpoint.String()),
				tagger.NewTagUnsafe("ip", peer.Endpoint.IP.String()),
				tagger.NewTagUnsafe("port", strconv.Itoa(peer.Endpoint.Port)),
				deviceTag,
			)
			c.conf.Tagger.Update(peer.PublicKeyHash,
				tagger.NewTagUnsafe("pub-key-sha1-7", peer.PublicKeyHash[:7]),
				tagger.NewTagUnsafe("allowed-ips", getAllowedIPsTag(peer.AllowedIPs)),
				tagger.NewTagUnsafe("endpoint", peer.Endpoint.String()),
				tagger.NewTagUnsafe("ip", peer.Endpoint.IP.String()),
				tagger.NewTagUnsafe("port", strconv.Itoa(peer.Endpoint.Port)),
				deviceTag,
			)
			tags := append(hostTags, c.conf.Tagger.GetUnstable(peer.PublicKey.String())...)
			_ = c.measures.CountWithNegativeReset(&metrics.Sample{
				Name:  wireguardMetricPrefix + "transfer.received",
				Value: float64(peer.ReceiveBytes),
				Time:  now,
				Host:  c.conf.Host,
				Tags:  tags,
			})
			_ = c.measures.CountWithNegativeReset(&metrics.Sample{
				Name:  wireguardMetricPrefix + "transfer.sent",
				Value: float64(peer.TransmitBytes),
				Time:  now,
				Host:  c.conf.Host,
				Tags:  tags,
			})
			age := now.Sub(peer.LastHandshakeTime)
			if age > time.Minute*5 {
				continue
			}
			c.measures.Gauge(&metrics.Sample{
				Name:  wireguardMetricPrefix + "handshake.age",
				Value: age.Seconds(),
				Time:  now,
				Host:  c.conf.Host,
				Tags:  tags,
			})
		}
	}
	c.measures.Purge()
	return nil
}