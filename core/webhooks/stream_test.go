package webhooks

import (
	"testing"
	"time"

	"github.com/owncast/owncast/core/chat/events"
	"github.com/owncast/owncast/core/data"
	"github.com/owncast/owncast/models"
)

func TestSendStreamStatusEvent(t *testing.T) {
	data.SetServerName("my server")
	data.SetServerSummary("my server where I stream")
	data.SetStreamTitle("my stream")

	streamID := "test-stream-id"

	checkPayload(t, models.StreamStarted, func() {
		sendStreamStatusEvent(events.StreamStarted, "id", streamID, time.Unix(72, 6).UTC())
	}, `{
		"id": "id",
		"name": "my server",
		"streamTitle": "my stream",
		"summary": "my server where I stream",
		"timestamp": "1970-01-01T00:01:12.000000006Z",
		"streamID": "test-stream-id",
		"status": {
			"lastConnectTime": null,
			"lastDisconnectTime": null,
			"online": true,
			"overallMaxViewerCount": 420,
			"sessionMaxViewerCount": 69,
			"streamTitle": "my stream",
			"versionNumber": "1.2.3",
			"viewerCount": 5
		}
	}`)
}
