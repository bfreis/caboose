package hostlink

import (
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

// The agent keeps to the limits agentproto names before it asks; they are
// the broker's, so a request it sends is never refused for its size.
func TestAskLimitsAreTheBrokers(t *testing.T) {
	for name, p := range map[string][2]int{
		"context":        {agentproto.MaxAskContext, broker.MaxContext},
		"options":        {agentproto.MaxAskOptions, broker.MaxOptions},
		"option":         {agentproto.MaxAskOption, broker.MaxOption},
		"recommendation": {agentproto.MaxAskRecommendation, broker.MaxRecommendation},
		"meanwhile":      {agentproto.MaxAskMeanwhile, broker.MaxMeanwhile},
		"session":        {agentproto.MaxAskSession, broker.MaxSession},
		"args":           {agentproto.MaxAskArgs, policy.MaxArgs},
	} {
		if p[0] != p[1] {
			t.Errorf("%s: agentproto %d, broker %d", name, p[0], p[1])
		}
	}
}
