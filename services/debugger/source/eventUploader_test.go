package sourcedebugger

import (
	"context"
	"time"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rudderlabs/rudder-server/config"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	mocksBackendConfig "github.com/rudderlabs/rudder-server/mocks/config/backend-config"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/pubsub"
	testutils "github.com/rudderlabs/rudder-server/utils/tests"
	"github.com/tidwall/gjson"
)

const (
	WriteKeyEnabled       = "enabled-write-key"
	WriteKeyEnabledNoUT   = "enabled-write-key-no-ut"
	WriteKeyEnabledOnlyUT = "enabled-write-key-only-ut"
	SourceIDEnabled       = "enabled-source"
	SourceIDDisabled      = "disabled-source"
	DestinationIDEnabledA = "enabled-destination-a" // test destination router
	DestinationIDEnabledB = "enabled-destination-b" // test destination batch router
	DestinationIDDisabled = "disabled-destination"
)

type eventUploaderContext struct {
	asyncHelper       testutils.AsyncTestHelper
	mockCtrl          *gomock.Controller
	configInitialised bool
	mockBackendConfig *mocksBackendConfig.MockBackendConfig
}

// Initiaze mocks and common expectations
func (c *eventUploaderContext) Setup() {
	c.mockCtrl = gomock.NewController(GinkgoT())
	c.mockBackendConfig = mocksBackendConfig.NewMockBackendConfig(c.mockCtrl)
	c.configInitialised = false
	Setup(c.mockBackendConfig)
}

func (c *eventUploaderContext) Finish() {
	c.mockCtrl.Finish()
}

func initEventUploader() {
	config.Reset()
	logger.Reset()
	Init()
}

var _ = Describe("eventUploader", func() {
	initEventUploader()

	var (
		c              *eventUploaderContext
		recordingEvent string
	)

	BeforeEach(func() {
		c = &eventUploaderContext{}
		c.Setup()
		recordingEvent = `{"t":"a"}`
		disableEventUploads = false

		tFunc := c.asyncHelper.ExpectAndNotifyCallback()

		c.mockBackendConfig.EXPECT().Subscribe(gomock.Any(), backendconfig.TopicProcessConfig).
			DoAndReturn(func(ctx context.Context, topic backendconfig.Topic) pubsub.DataChannel {
				tFunc()

				ch := make(chan pubsub.DataEvent, 1)
				ch <- pubsub.DataEvent{Data: map[string]backendconfig.ConfigT{workspaceID: sampleBackendConfig}, Topic: string(topic)}
				// on Subscribe, emulate a backend configuration event
				c.configInitialised = true

				go func() {
					<-ctx.Done()
					close(ch)
				}()
				return ch
			})
	})

	AfterEach(func() {
		c.Finish()
	})

	Context("RecordEvent", func() {
		It("returns false if disableEventUploads is true", func() {
			c.asyncHelper.WaitWithTimeout(5 * time.Second)
			disableEventUploads = true
			Expect(RecordEvent(sampleWriteKey, []byte(recordingEvent))).To(BeFalse())
		})

		It("returns false if writeKey is not part of uploadEnabledWriteKeys", func() {
			c.asyncHelper.WaitWithTimeout(5 * time.Second)
			Expect(RecordEvent(sampleWriteKey, []byte(recordingEvent))).To(BeFalse())
		})

		It("transforms payload properly", func() {
			c.asyncHelper.WaitWithTimeout(5 * time.Second)
			recordingEvent0 := `{"receivedAt":"2021-08-03T17:26:","writeKey":"1vWezJfHKkbUHexNepDsGcSVWae","requestIP":"[::1]",  "batch": [{"anonymousId":"anon_id","channel":"android-sdk","context":{"app":{"build":"1","name":"RudderAndroidClient","namespace":"com.rudderlabs.android.sdk","version":"1.0"},"device":{"id":"49e4bdd1c280bc00","manufacturer":"Google","model":"Android SDK built for x86","name":"generic_x86"},"library":{"name":"com.rudderstack.android.sdk.core"},"locale":"en-US","network":{"carrier":"Android"},"screen":{"density":420,"height":1794,"width":1080},"traits":{"anonymousId":"49e4bdd1c280bc00"},"user_agent":"Dalvik/2.1.0 (Linux; U; Android 9; Android SDK built for x86 Build/PSR1.180720.075)"},"event":{"name": "Demo Track"},"integrations":{"All":true},"messageId":"7a355fdd-0325-4778-9905-b43f586acdd4","originalTimestamp":"2019-08-12T05:08:30.909Z","properties":{"category":"Demo Category","floatVal":4.501,"label":"Demo Label","testArray":[{"id":"elem1","value":"e1"},{"id":"elem2","value":"e2"}],"testMap":{"t1":"a","t2":4},"value":5},"rudderId":"90ca6da0-292e-4e79-9880-f8009e0ae4a3","sentAt":"2019-08-12T05:08:30.909Z","type":"track"}]}`
			recordingEvent = `{"receivedAt":"2021-08-03T17:26:00.279+05:30","writeKey":"1vWezJfHKkbUHexNepDsGcSVWae","requestIP":"[::1]",  "batch": [{"anonymousId":"anon_id","channel":"android-sdk","context":{"app":{"build":"1","name":"RudderAndroidClient","namespace":"com.rudderlabs.android.sdk","version":"1.0"},"device":{"id":"49e4bdd1c280bc00","manufacturer":"Google","model":"Android SDK built for x86","name":"generic_x86"},"library":{"name":"com.rudderstack.android.sdk.core"},"locale":"en-US","network":{"carrier":"Android"},"screen":{"density":420,"height":1794,"width":1080},"traits":{"anonymousId":"49e4bdd1c280bc00"},"user_agent":"Dalvik/2.1.0 (Linux; U; Android 9; Android SDK built for x86 Build/PSR1.180720.075)"},"event":"Demo Track","integrations":{"All":true},"messageId":"7a355fdd-0325-4778-9905-b43f586acdd4","originalTimestamp":"2019-08-12T05:08:30.909Z","properties":{"category":"Demo Category","floatVal":4.501,"label":"Demo Label","testArray":[{"id":"elem1","value":"e1"},{"id":"elem2","value":"e2"}],"testMap":{"t1":"a","t2":4},"value":5},"rudderId":"90ca6da0-292e-4e79-9880-f8009e0ae4a3","sentAt":"2019-08-12T05:08:30.909Z","type":"track"}]}`
			var eventUploader EventUploader
			var payload []*GatewayEventBatchT
			payload = append(payload, &GatewayEventBatchT{writeKey: WriteKeyEnabled, eventBatch: []byte(recordingEvent0)})
			payload = append(payload, &GatewayEventBatchT{writeKey: WriteKeyEnabled, eventBatch: []byte(recordingEvent)})
			rawJson, err := eventUploader.Transform(payload)
			Expect(err).To(BeNil())
			Expect(gjson.GetBytes(rawJson, `1vWezJfHKkbUHexNepDsGcSVWae.0.eventName`).String()).To(Equal("{\"name\":\"Demo Track\"}"))
			Expect(gjson.GetBytes(rawJson, `1vWezJfHKkbUHexNepDsGcSVWae.1.eventName`).String()).To(Equal("Demo Track"))
			Expect(gjson.GetBytes(rawJson, `1vWezJfHKkbUHexNepDsGcSVWae.1.eventType`).String()).To(Equal("track"))
		})

		It("ignores improperly built payload", func() {
			c.asyncHelper.WaitWithTimeout(5 * time.Second)
			recordingEvent0 := `{"receivedAt":"2021-08-03T17:26:","writeKey":"1vWezJfHKkbUHexNepDsGcSVWae","requestIP":"[::1]",  "batch": [{"anonymousId":"anon_id","channel":"android-sdk","context":{"app":{"build":"1","name":"RudderAndroidClient","namespace":"com.rudderlabs.android.sdk","version":"1.0"},"device":{"id":"49e4bdd1c280bc00","manufacturer":"Google","model":"Android SDK built for x86","name":"generic_x86"},"library":{"name":"com.rudderstack.android.sdk.core"},"locale":"en-US","network":{"carrier":"Android"},"screen":{"density":420,"height":1794,"width":1080},"traits":{"anonymousId":"49e4bdd1c280bc00"},"user_agent":"Dalvik/2.1.0 (Linux; U; Android 9; Android SDK built for x86 Build/PSR1.180720.075)"},"event":{"name": "Demo Track"},"integrations":{"All":true},"messageId":"7a355fdd-0325-4778-9905-b43f586acdd4","originalTimestamp":"2019-08-12T05:08:30.909Z","properties":{"category":"Demo Category","floatVal":4.501,"label":"Demo Label","testArray":[{"id":"elem1","value":"e1"},{"id":"elem2","value":"e2"}],"testMap":{"t1":"a","t2":4},"value":5},"rudderId":"90ca6da0-292e-4e79-9880-f8009e0ae4a3","sentAt":"2019-08-12T05:08:30.909Z","type":"track"}`
			var eventUploader EventUploader
			var payload []*GatewayEventBatchT
			payload = append(payload, &GatewayEventBatchT{writeKey: WriteKeyEnabled, eventBatch: []byte(recordingEvent0)})
			rawJson, err := eventUploader.Transform(payload)
			Expect(err).To(BeNil())
			Expect(string(rawJson)).To(Equal(`{"version":"v2"}`))
		})

		It("records events", func() {
			c.asyncHelper.WaitWithTimeout(5 * time.Second)
			recordingEvent = `{"receivedAt":"2021-08-03T17:26:00.279+05:30","writeKey":"1vWezJfHKkbUHexNepDsGcSVWae","requestIP":"[::1]",  "batch": [{"anonymousId":"anon_id","channel":"android-sdk","context":{"app":{"build":"1","name":"RudderAndroidClient","namespace":"com.rudderlabs.android.sdk","version":"1.0"},"device":{"id":"49e4bdd1c280bc00","manufacturer":"Google","model":"Android SDK built for x86","name":"generic_x86"},"library":{"name":"com.rudderstack.android.sdk.core"},"locale":"en-US","network":{"carrier":"Android"},"screen":{"density":420,"height":1794,"width":1080},"traits":{"anonymousId":"49e4bdd1c280bc00"},"user_agent":"Dalvik/2.1.0 (Linux; U; Android 9; Android SDK built for x86 Build/PSR1.180720.075)"},"event":"Demo Track","integrations":{"All":true},"messageId":"7a355fdd-0325-4778-9905-b43f586acdd4","originalTimestamp":"2019-08-12T05:08:30.909Z","properties":{"category":"Demo Category","floatVal":4.501,"label":"Demo Label","testArray":[{"id":"elem1","value":"e1"},{"id":"elem2","value":"e2"}],"testMap":{"t1":"a","t2":4},"value":5},"rudderId":"90ca6da0-292e-4e79-9880-f8009e0ae4a3","sentAt":"2019-08-12T05:08:30.909Z","type":"track"}]}`
			eventuallyFunc := func() bool { return RecordEvent(WriteKeyEnabled, []byte(recordingEvent)) }
			Eventually(eventuallyFunc).Should(BeTrue())
		})
	})
})

type sourceConfigMap map[string]interface{}

var workspaceID = "workspace"

var sampleBackendConfig = backendconfig.ConfigT{
	WorkspaceID: workspaceID,
	Sources: []backendconfig.SourceT{
		{
			ID:       SourceIDDisabled,
			WriteKey: WriteKeyEnabled,
			Enabled:  false,
		},
		{
			ID:       SourceIDEnabled,
			WriteKey: WriteKeyEnabled,
			Enabled:  true,
			Config:   sourceConfigMap{"eventUpload": true},
			Destinations: []backendconfig.DestinationT{
				{
					ID:                 DestinationIDEnabledA,
					Name:               "A",
					Enabled:            true,
					IsProcessorEnabled: true,
					DestinationDefinition: backendconfig.DestinationDefinitionT{
						ID:          "enabled-destination-a-definition-id",
						Name:        "enabled-destination-a-definition-name",
						DisplayName: "enabled-destination-a-definition-display-name",
						Config:      map[string]interface{}{},
					},
				},
				{
					ID:                 DestinationIDEnabledB,
					Name:               "B",
					Enabled:            true,
					IsProcessorEnabled: true,
					DestinationDefinition: backendconfig.DestinationDefinitionT{
						ID:          "enabled-destination-b-definition-id",
						Name:        "MINIO",
						DisplayName: "enabled-destination-b-definition-display-name",
						Config:      map[string]interface{}{},
					},
					Transformations: []backendconfig.TransformationT{
						{
							VersionID: "transformation-version-id",
						},
					},
				},
				// This destination should receive no events
				{
					ID:                 DestinationIDDisabled,
					Name:               "C",
					Enabled:            false,
					IsProcessorEnabled: true,
					DestinationDefinition: backendconfig.DestinationDefinitionT{
						ID:          "destination-definition-disabled",
						Name:        "destination-definition-name-disabled",
						DisplayName: "destination-definition-display-name-disabled",
						Config:      map[string]interface{}{},
					},
				},
			},
		},
		{
			ID:       SourceIDEnabled,
			WriteKey: WriteKeyEnabledNoUT,
			Enabled:  true,
			Destinations: []backendconfig.DestinationT{
				{
					ID:                 DestinationIDEnabledA,
					Name:               "A",
					Enabled:            true,
					IsProcessorEnabled: true,
					DestinationDefinition: backendconfig.DestinationDefinitionT{
						ID:          "enabled-destination-a-definition-id",
						Name:        "enabled-destination-a-definition-name",
						DisplayName: "enabled-destination-a-definition-display-name",
						Config:      map[string]interface{}{},
					},
				},
				// This destination should receive no events
				{
					ID:                 DestinationIDDisabled,
					Name:               "C",
					Enabled:            false,
					IsProcessorEnabled: true,
					DestinationDefinition: backendconfig.DestinationDefinitionT{
						ID:          "destination-definition-disabled",
						Name:        "destination-definition-name-disabled",
						DisplayName: "destination-definition-display-name-disabled",
						Config:      map[string]interface{}{},
					},
				},
			},
		},
		{
			ID:       SourceIDEnabled,
			WriteKey: WriteKeyEnabledOnlyUT,
			Enabled:  true,
			Destinations: []backendconfig.DestinationT{
				{
					ID:                 DestinationIDEnabledB,
					Name:               "B",
					Enabled:            true,
					IsProcessorEnabled: true,
					DestinationDefinition: backendconfig.DestinationDefinitionT{
						ID:          "enabled-destination-b-definition-id",
						Name:        "MINIO",
						DisplayName: "enabled-destination-b-definition-display-name",
						Config:      map[string]interface{}{},
					},
					Transformations: []backendconfig.TransformationT{
						{
							VersionID: "transformation-version-id",
						},
					},
				},
			},
		},
	},
}

var sampleWriteKey = "random_write_key"
