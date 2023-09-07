package pusher

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const (
	pingPayload = `{"event":"pusher:ping","data":"{}"}`
	pongPayload = `{"event":"pusher:pong","data":"{}"}`

	pusherPing                  = "pusher:ping"
	pusherPong                  = "pusher:pong"
	pusherError                 = "pusher:error"
	pusherSubscribe             = "pusher:subscribe"
	pusherUnsubscribe           = "pusher:unsubscribe"
	pusherConnEstablished       = "pusher:connection_established"
	pusherSubSucceeded          = "pusher:subscription_succeeded"
	pusherInternalSubSucceeded  = "pusher_internal:subscription_succeeded"
	pusherInternalMemberAdded   = "pusher_internal:member_added"
	pusherInternalMemberRemoved = "pusher_internal:member_removed"

	localOrigin = "http://localhost/"

	connURLFormat     = "%s://%s:%d/app/%s?protocol=%s"
	secureScheme      = "wss"
	securePort        = 443
	insecureScheme    = "ws"
	insecurePort      = 80
	defaultHost       = "ws.pusherapp.com"
	clusterHostFormat = "ws-%s.pusher.com"
	protocolVersion   = "7"
)

type boundEventChans map[chan Event]struct{}

type subscribedChannels map[string]internalChannel

// Client represents a Pusher websocket client. After creating an instance, it
// is necessary to call Connect to establish the connection with Pusher. Calling
// any other methods before a connection is established is an invalid operation
// and may panic.
type Client struct {
	// The cluster to connect to. The default is Pusher's "mt1" cluster in the
	// "us-east-1" region.
	Cluster string
	// Whether to connect to Pusher over an insecure websocket connection.
	Insecure bool

	// The URL to call when authenticating private or presence channels.
	AuthURL string
	// Additional parameters to be sent in the POST body of an authentication request.
	AuthParams url.Values
	// Additional HTTP headers to be sent in an authentication request.
	AuthHeaders http.Header

	// If provided, errors that occur while receiving messages and errors emitted
	// by Pusher will be sent to this channel.
	Errors chan error

	socketID string
	// TODO: make this configurable
	activityTimeout time.Duration
	// TODO: implement timeout logic
	// pongTimeout time.Duration

	ws                 *websocket.Conn
	connected          bool
	activityTimer      *time.Timer
	activityTimerReset chan struct{}
	boundEvents        map[string]boundEventChans
	// TODO: implement global bindings
	// globalBindings     boundEventChans
	subscribedChannels subscribedChannels

	mutex sync.RWMutex

	// used for testing
	OverrideHost string
	OverridePort int
}

type connectionData struct {
	SocketID        string `json:"socket_id"`
	ActivityTimeout int    `json:"activity_timeout"`
}

// UnmarshalDataString is a convenience function to unmarshal double-encoded
// JSON data from a Pusher event. See https://pusher.com/docs/pusher_protocol#double-encoding
func UnmarshalDataString(data json.RawMessage, dest interface{}) error {
	var dataStr string
	err := json.Unmarshal(data, &dataStr)
	if err != nil {
		return err
	}

	return json.Unmarshal([]byte(dataStr), dest)
}

func (c *Client) generateConnURL(appKey string) string {
	scheme, port := secureScheme, securePort
	if c.Insecure {
		scheme, port = insecureScheme, insecurePort
	}
	if c.OverridePort != 0 {
		port = c.OverridePort
	}

	host := defaultHost
	if c.Cluster != "" {
		host = fmt.Sprintf(clusterHostFormat, c.Cluster)
	}
	if c.OverrideHost != "" {
		host = c.OverrideHost
	}

	return fmt.Sprintf(connURLFormat, scheme, host, port, appKey, protocolVersion)
}

// Connect establishes a connection to the Pusher app specified by appKey.
func (c *Client) Connect(appKey string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	var err error
	c.ws, err = websocket.Dial(c.generateConnURL(appKey), "", localOrigin)
	if err != nil {
		return err
	}

	var event Event
	err = websocket.JSON.Receive(c.ws, &event)
	if err != nil {
		return err
	}

	switch event.Event {
	case pusherError:
		return extractEventError(event)
	case pusherConnEstablished:
		var connData connectionData
		err = UnmarshalDataString(event.Data, &connData)
		if err != nil {
			return err
		}
		c.connected = true
		c.socketID = connData.SocketID
		c.activityTimeout = time.Duration(connData.ActivityTimeout) * time.Second
		c.activityTimer = time.NewTimer(c.activityTimeout)
		c.activityTimerReset = make(chan struct{}, 1)
		c.boundEvents = map[string]boundEventChans{}
		c.subscribedChannels = subscribedChannels{}

		go c.heartbeat()
		go c.listen()

		return nil
	default:
		return fmt.Errorf("Got unknown event type from Pusher: %s", event.Event)
	}
}

func (c *Client) GetSocketID() string {
	return c.socketID
}

func (c *Client) isConnected() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	return c.connected
}

func (c *Client) resetActivityTimer() {
	select {
	case c.activityTimerReset <- struct{}{}:
		return
	default:
		// Timer reset is already requested.
	}
}

func (c *Client) heartbeat() {
	for c.isConnected() {
		select {
		case <-c.activityTimerReset:
			if !c.activityTimer.Stop() {
				select {
				case <-c.activityTimer.C: //try to drain from the channel
				default:
				}
			}
			c.activityTimer.Reset(c.activityTimeout)

		case <-c.activityTimer.C:
			websocket.Message.Send(c.ws, pingPayload)
			// TODO: implement timeout/reconnect logic
		}
	}
}

func (c *Client) sendError(err error) {
	select {
	case c.Errors <- err:
	default:
	}
}

func (c *Client) listen() {
	for c.isConnected() {
		var event Event
		err := websocket.JSON.Receive(c.ws, &event)
		if err != nil {
			// If the websocket connection was closed, Receive will return an error.
			// This is expected for an explicit disconnect.
			if !c.isConnected() {
				return
			}
			c.sendError(err)
			continue
		}

		c.resetActivityTimer()

		switch event.Event {
		case pusherPing:
			websocket.Message.Send(c.ws, pongPayload)
		case pusherPong:
			// TODO: stop pong timeout timer
		case pusherError:
			c.sendError(extractEventError(event))
		default:
			c.mutex.RLock()
			for boundChan := range c.boundEvents[event.Event] {
				go func(boundChan chan Event, event Event) {
					boundChan <- event
				}(boundChan, event)
			}
			if subChan, ok := c.subscribedChannels[event.Channel]; ok {
				subChan.handleEvent(event.Event, event.Data)
			}
			c.mutex.RUnlock()
		}
	}
}

// Subscribe creates a subscription to the specified channel. Authentication
// will be attempted for private and presence channels. If the channel has
// already been subscribed, this method will return the existing Channel
// instance.
//
// A channel is always returned, regardless of any errors. The error value
// indicates if the subscription succeeded. Failed subscriptions may be retried
// with `Channel.Subscribe()`.
//
// See SubscribePresence() for presence channels.
func (c *Client) Subscribe(channelName string, opts ...SubscribeOption) (Channel, error) {
	c.mutex.RLock()
	ch, ok := c.subscribedChannels[channelName]
	c.mutex.RUnlock()

	if !ok {
		baseChan := &channel{
			name:        channelName,
			boundEvents: map[string]boundDataChans{},
			client:      c,
		}
		switch {
		case strings.HasPrefix(channelName, "private-"):
			ch = &privateChannel{baseChan}
		case strings.HasPrefix(channelName, "presence-"):
			ch = newPresenceChannel(baseChan)
		default:
			ch = baseChan
		}
		c.mutex.Lock()
		c.subscribedChannels[channelName] = ch
		c.mutex.Unlock()
	}

	return ch, ch.Subscribe(opts...)
}

// SubscribePresence creates a subscription to the specified presence channel.
// If the channel has already been subscribed, this method will return the
// existing channel instance.
//
// A channel is always returned, regardless of any errors. The error value
// indicates if the subscription succeeded. Failed subscriptions may be retried
// with `Channel.Subscribe()`.
//
// An error is returned if channelName is not a presence channel. Use
// Subscribe() for other channel types.
func (c *Client) SubscribePresence(channelName string, opts ...SubscribeOption) (PresenceChannel, error) {
	if !strings.HasPrefix(channelName, "presence-") {
		return nil, fmt.Errorf("invalid presence channel name, must start with 'presence-': %s", channelName)
	}

	ch, subscribeErr := c.Subscribe(channelName, opts...)
	return ch.(*presenceChannel), subscribeErr
}

// Unsubscribe unsubscribes from the specified channel. Events will no longer
// be received from that channe. Note that a nil error does not mean that the
// unsubscription was successful, just that the request was sent.
func (c *Client) Unsubscribe(channelName string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	ch, ok := c.subscribedChannels[channelName]
	if !ok {
		return nil
	}

	delete(c.subscribedChannels, channelName)
	return ch.Unsubscribe()
}

// Bind returns a channel to which all matching events received on the connection
// will be sent.
func (c *Client) Bind(event string) chan Event {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	boundChan := make(chan Event)

	if c.boundEvents[event] == nil {
		c.boundEvents[event] = boundEventChans{}
	}
	c.boundEvents[event][boundChan] = struct{}{}

	return boundChan
}

// Unbind removes bindings for an event. If chans are passed, only those bindings
// will be removed. Otherwise, all bindings for an event will be removed.
func (c *Client) Unbind(event string, chans ...chan Event) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if len(chans) == 0 {
		delete(c.boundEvents, event)
		return
	}

	eventBoundChans := c.boundEvents[event]
	for _, boundChan := range chans {
		delete(eventBoundChans, boundChan)
	}
}

// SendEvent sends an event on the Pusher connection.
func (c *Client) SendEvent(event string, data interface{}, channelName string) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return err
	}

	e := Event{
		Event:   event,
		Data:    dataJSON,
		Channel: channelName,
	}

	c.resetActivityTimer()

	return websocket.JSON.Send(c.ws, e)
}

// Disconnect closes the websocket connection to Pusher. Any subsequent operations
// are invalid until Connect is called again.
func (c *Client) Disconnect() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.connected = false

	return c.ws.Close()
}
