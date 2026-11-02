package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPublishEncodesOnceForRingAndSubscribers(t *testing.T) {
	encoder := NewMockEventEncoder(t)
	raw := json.RawMessage(`{"type":"event","session":"owned","seq":1,"kind":"info","data":{}}`)
	encoder.EXPECT().EncodeNotification(mock.Anything).RunAndReturn(func(ev Event) (json.RawMessage, error) {
		require.Equal(t, ID("owned"), ev.Session)
		require.Equal(t, uint64(1), ev.Seq)
		return raw, nil
	}).Once()
	c := &Controller{id: "owned", deps: Deps{Clock: time.Now, EventEncoder: encoder}, ring: &eventRing{}, subs: make(map[uint64]*subscription)}
	for i := range 7 {
		c.subs[uint64(i)] = &subscription{events: make(chan Event, 2)}
	}
	c.publish(EvInfo, InfoEvent{})
	require.Equal(t, len(raw), c.ring.bytes)
	replay, err := c.ring.replay(0, 1)
	require.NoError(t, err)
	require.Same(t, &raw[0], &replay[0].Notification[0])
	for _, sub := range c.subs {
		ev := <-sub.events
		require.Same(t, &raw[0], &ev.Notification[0])
		require.Equal(t, raw, ev.Notification)
	}
}
