package agent

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mxcd/inkling-chat/internal/model"
)

func TestTheTimingsAreReadFromAStreamThatArrivesInPieces(t *testing.T) {
	t.Parallel()
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"the word \\\"timings\\\"\"}}]}\n\n" +
		": keep-alive\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9},\"timings\":{\"prompt_n\":7,\"prompt_ms\":1040.5,\"prompt_per_second\":2030,\"predicted_per_second\":95.5,\"draft_n\":10,\"draft_n_accepted\":4}}\n\n" +
		"data: [DONE]\n\n"
	log := &timingsLog{}
	body := &timingsBody{ReadCloser: io.NopCloser(iotest.OneByteReader(strings.NewReader(stream))), log: log, call: log.answered()}

	passed, err := io.ReadAll(body)

	require.NoError(t, err)
	require.Equal(t, stream, string(passed), "the body reaches aikido unchanged")
	require.Equal(t, &model.ServerTimings{
		PromptProcessing: 1040500 * time.Microsecond, PromptTokens: 7, PromptTokensPerSecond: 2030, GenerationTokensPerSecond: 95.5, DraftTokens: 10, DraftAccepted: 4,
	}, log.next())
	require.Nil(t, log.next(), "one answered call, one timings")
}
