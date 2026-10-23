package agent

import (
	"context"
	"io"
	"net"
	"time"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/agentproto"
)

// The link's side of `caboose-agent request` and `requests`: it passes the
// asks and the listings to the host, bounded again whatever the command
// sent (any process in the sandbox can talk to the socket), keeps the
// presence the commands report, and tells the host the count.

// brokerOff is what a request or a listing gets from a host that files no
// requests.
const brokerOff = "the host files no requests for this sandbox: its caboose is older than this sandbox's, " +
	"or nothing on it answers them ('caboose requests' on the host lists what waits)"

// waitSlack is how much longer than its wait the link waits for the host's
// answer to a listing.
const waitSlack = 20 * time.Second

// clipBytes cuts s to at most n bytes, at a character boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// boundAsk is a copy of a within agentproto's bounds, of the kind
// and shape the host expects.
func boundAsk(a agentproto.Ask) agentproto.Ask {
	out := agentproto.Ask{
		Kind:           clipBytes(a.Kind, 32),
		Alias:          clipBytes(a.Alias, 64),
		Host:           clipBytes(a.Host, agentproto.MaxEgressHost),
		Port:           a.Port,
		URL:            clipBytes(a.URL, 2048),
		Context:        clipBytes(a.Context, agentproto.MaxAskContext),
		Recommendation: clipBytes(a.Recommendation, agentproto.MaxAskRecommendation),
		Meanwhile:      clipBytes(a.Meanwhile, agentproto.MaxAskMeanwhile),
		Session:        clipBytes(a.Session, agentproto.MaxAskSession),
	}
	for i, o := range a.Options {
		if i == agentproto.MaxAskOptions {
			break
		}
		out.Options = append(out.Options, clipBytes(o, agentproto.MaxAskOption))
	}
	if len(a.Args) > 0 {
		out.Args = map[string]string{}
		for k, v := range a.Args {
			if len(out.Args) == agentproto.MaxAskArgs {
				break
			}
			out.Args[clipBytes(k, 64)] = clipBytes(v, 256)
		}
	}
	return out
}

// brokerClient serves a client's OpBrokerRequest or OpBrokerRequests.
func (l *Link) brokerClient(c net.Conn, m agentproto.Message) agentproto.Message {
	l.waitHelloed()
	l.mu.Lock()
	on := l.broker
	l.mu.Unlock()
	if !on {
		return agentproto.Message{Type: agentproto.TypeResponse, Error: brokerOff, Reason: agentproto.AliasUnavailable}
	}
	if m.Op == agentproto.OpBrokerRequest {
		if m.Ask == nil {
			return agentproto.Message{Type: agentproto.TypeResponse, Error: "the request names nothing to ask for"}
		}
		a := boundAsk(*m.Ask)
		return l.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &a})
	}
	wait := min(max(m.Wait, 0), agentproto.MaxWait)
	if m.ModPID > 0 {
		defer l.presence.join(m.ModPID)()
	}
	// The wait ends when the client goes away: nothing else reaches this
	// connection after its request, so a read returns only when it closes.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, c)
		cancel()
	}()
	return l.requestCtx(ctx, agentproto.Message{Op: agentproto.OpBrokerRequests, Since: m.Since, Wait: wait,
		Session: clipBytes(m.Session, agentproto.MaxAskSession), All: m.All}, time.Duration(wait)*time.Second+waitSlack)
}

// watchMods tells the host how many Claude Code sessions run in the
// sandbox, and how many have caboose's mod, once the hello says it files
// requests, and again when that changes: read every scanEvery, sent at most
// every ModsEvery.
func (l *Link) watchMods() {
	l.waitHelloed()
	l.mu.Lock()
	on := l.broker
	l.mu.Unlock()
	if !on {
		return
	}
	t := time.NewTicker(l.scanEvery)
	defer t.Stop()
	for {
		if info, send := l.presence.due(); send {
			if l.sess.Send(agentproto.Message{Type: agentproto.TypeMods, Mods: &info}) != nil {
				return
			}
		}
		select {
		case <-t.C:
		case <-l.sess.Done():
			return
		}
	}
}
