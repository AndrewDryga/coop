package acpproxy

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/threadtext"
)

func TestThreadMessagesAreStructured(t *testing.T) {
	sign := "Sign in to grok@default on the host:\n\n```\ncoop login grok@default\n```"
	for name, text := range map[string]string{
		"no box":          sessionEndedNoBoxText,
		"replay failed":   replayFailedText("The provider refused."),
		"replay, sign in": replayFailedText(sign),
		"no such method":  errorMessage(parse(authenticationUnavailableResponse("1", "grok", "x")).Error),
	} {
		t.Run(name, func(t *testing.T) { threadtext.Check(t, text) })
	}
	line := parse(SessionMessageLine("S", sign))
	if !strings.Contains(string(line.Params), `"text":"\n\nSign in`) || !strings.Contains(string(line.Params), "```\\n\\n\"") {
		t.Errorf("a message must be set apart from what is around it, or its code block never closes: %s", line.Params)
	}
}
