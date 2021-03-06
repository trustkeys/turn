package turn

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/gortc/stun"
)

// Client for TURN server.
//
// Provides transparent net.Conn interfaces to remote peers.
type Client struct {
	log         *zap.Logger
	con         net.Conn
	stun        STUNClient
	mux         sync.RWMutex
	username    stun.Username
	password    string
	realm       stun.Realm
	integrity   stun.MessageIntegrity
	alloc       *Allocation // the only allocation
	refreshRate time.Duration
}

// ClientOptions contains available config for TURN  client.
type ClientOptions struct {
	Conn net.Conn
	STUN STUNClient  // optional STUN client
	Log  *zap.Logger // defaults to Nop

	// Long-term integrity.
	Username string
	Password string

	// STUN client options.
	RTO          time.Duration
	NoRetransmit bool

	// TURN options.
	RefreshRate     time.Duration
	RefreshDisabled bool
}

// RefreshRate returns current rate of refresh requests.
func (c *Client) RefreshRate() time.Duration { return c.refreshRate }

const defaultRefreshRate = time.Minute

// NewClient creates and initializes new TURN client.
func NewClient(o ClientOptions) (*Client, error) {
	if o.Conn == nil {
		return nil, errors.New("connection not provided")
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	c := &Client{
		password: o.Password,
		log:      o.Log,
	}
	if o.STUN == nil {
		// Setting up de-multiplexing.
		m := newMultiplexer(o.Conn, c.log.Named("multiplexer"))
		go m.discardData() // discarding any non-stun/turn data
		o.Conn = bypassWriter{
			reader: m.turnL,
			writer: m.conn,
		}
		// Starting STUN client on multiplexed connection.
		var err error
		stunOptions := []stun.ClientOption{
			stun.WithHandler(c.stunHandler),
		}
		if o.NoRetransmit {
			stunOptions = append(stunOptions, stun.WithNoRetransmit)
		}
		if o.RTO > 0 {
			stunOptions = append(stunOptions, stun.WithRTO(o.RTO))
		}
		o.STUN, err = stun.NewClient(bypassWriter{
			reader: m.stunL,
			writer: m.conn,
		}, stunOptions...)
		if err != nil {
			return nil, err
		}
	}
	c.stun = o.STUN
	c.con = o.Conn
	c.refreshRate = defaultRefreshRate
	if o.RefreshRate > 0 {
		c.refreshRate = o.RefreshRate
	}
	if o.RefreshDisabled {
		c.refreshRate = 0
	}
	if o.Username != "" {
		c.username = stun.NewUsername(o.Username)
	}
	go c.readUntilClosed()
	return c, nil
}

// STUNClient abstracts STUN protocol interaction.
type STUNClient interface {
	Indicate(m *stun.Message) error
	Do(m *stun.Message, f func(e stun.Event)) error
}

func (c *Client) stunHandler(e stun.Event) {
	if e.Error != nil {
		// Just ignoring.
		return
	}
	if e.Message.Type != stun.NewType(stun.MethodData, stun.ClassIndication) {
		return
	}
	var (
		data Data
		addr PeerAddress
	)
	if err := e.Message.Parse(&data, &addr); err != nil {
		c.log.Error("failed to parse while handling incoming STUN message", zap.Error(err))
		return
	}
	c.mux.RLock()
	for i := range c.alloc.perms {
		if !Addr(c.alloc.perms[i].peerAddr).Equal(Addr(addr)) {
			continue
		}
		if _, err := c.alloc.perms[i].peerL.Write(data); err != nil {
			c.log.Error("failed to write", zap.Error(err))
		}
	}
	c.mux.RUnlock()
}

// ZapChannelNumber returns zap.Field for ChannelNumber.
func ZapChannelNumber(key string, v ChannelNumber) zap.Field {
	return zap.String(key, fmt.Sprintf("0x%x", int(v)))
}

func (c *Client) handleChannelData(data *ChannelData) {
	c.log.Debug("handleChannelData", ZapChannelNumber("number", data.Number))
	c.mux.RLock()
	for i := range c.alloc.perms {
		if data.Number != c.alloc.perms[i].Binding() {
			continue
		}
		if _, err := c.alloc.perms[i].peerL.Write(data.Data); err != nil {
			c.log.Error("failed to write", zap.Error(err))
		}
	}
	c.mux.RUnlock()
}

func (c *Client) readUntilClosed() {
	buf := make([]byte, 1500)
	for {
		n, err := c.con.Read(buf)
		if err != nil {
			if err == io.EOF {
				continue
			}
			c.log.Error("read failed", zap.Error(err))
			break
		}
		data := buf[:n]
		if !IsChannelData(data) {
			continue
		}
		cData := &ChannelData{
			Raw: make([]byte, n),
		}
		copy(cData.Raw, data)
		if err := cData.Decode(); err != nil {
			panic(err)
		}
		go c.handleChannelData(cData)
	}
}

func (c *Client) sendData(buf []byte, peerAddr *PeerAddress) (int, error) {
	err := c.stun.Indicate(stun.MustBuild(stun.TransactionID,
		stun.NewType(stun.MethodSend, stun.ClassIndication),
		Data(buf), peerAddr,
	))
	if err == nil {
		return len(buf), nil
	}
	return 0, err
}

func (c *Client) sendChan(buf []byte, n ChannelNumber) (int, error) {
	if !n.Valid() {
		return 0, ErrInvalidChannelNumber
	}
	d := &ChannelData{
		Data:   buf,
		Number: n,
	}
	d.Encode()
	return c.con.Write(d.Raw)
}

func (c *Client) do(req, res *stun.Message) error {
	var stunErr error
	if doErr := c.stun.Do(req, func(e stun.Event) {
		if e.Error != nil {
			stunErr = e.Error
			return
		}
		if res == nil {
			return
		}
		if err := e.Message.CloneTo(res); err != nil {
			stunErr = err
		}
	}); doErr != nil {
		return doErr
	}
	return stunErr
}
