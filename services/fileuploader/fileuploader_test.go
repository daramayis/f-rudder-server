package fileuploader

import (
	"context"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	mock_backendconfig "github.com/rudderlabs/rudder-server/mocks/config/backend-config"
	"github.com/rudderlabs/rudder-server/utils/pubsub"
)

func TestFileUploaderUpdatingWithConfigBackend(t *testing.T) {
	RegisterTestingT(t)
	ctrl := gomock.NewController(t)
	config := mock_backendconfig.NewMockBackendConfig(ctrl)

	configCh := make(chan pubsub.DataEvent)

	var ready sync.WaitGroup
	ready.Add(2)

	var storageSettings sync.WaitGroup
	storageSettings.Add(1)

	config.EXPECT().Subscribe(
		gomock.Any(),
		gomock.Eq(backendconfig.TopicBackendConfig),
	).DoAndReturn(func(ctx context.Context, topic backendconfig.Topic) pubsub.DataChannel {
		ready.Done()
		go func() {
			<-ctx.Done()
			close(configCh)
		}()

		return configCh
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Given I have a fileUploaderProvider reading from the backend
	fileUploaderProvider := NewProvider(ctx, config)
	var err error
	var preferences backendconfig.StoragePreferences

	go func() {
		ready.Done()
		preferences, err = fileUploaderProvider.GetStoragePreferences("testWorkspaceId-1")
		storageSettings.Done()
	}()

	// When the config backend has not published any event yet
	ready.Wait()
	Expect(preferences).To(BeEquivalentTo(backendconfig.StoragePreferences{}))

	// When user has not configured any storage
	configCh <- pubsub.DataEvent{
		Data: map[string]backendconfig.ConfigT{
			"testWorkspaceId-1": {
				WorkspaceID: "testWorkspaceId-1",
				Settings: backendconfig.Settings{
					DataRetention: backendconfig.DataRetention{
						UseSelfStorage: false,
						StorageBucket:  backendconfig.StorageBucket{},
						StoragePreferences: backendconfig.StoragePreferences{
							ProcErrors:   true,
							GatewayDumps: false,
						},
					},
				},
			},
			"testWorkspaceId-2": {
				WorkspaceID: "testWorkspaceId-2",
				Settings: backendconfig.Settings{
					DataRetention: backendconfig.DataRetention{
						UseSelfStorage: true,
						StorageBucket: backendconfig.StorageBucket{
							Type:   "",
							Config: map[string]interface{}{},
						},
						StoragePreferences: backendconfig.StoragePreferences{
							ProcErrors:   false,
							GatewayDumps: false,
						},
					},
				},
			},
		},
		Topic: string(backendconfig.TopicBackendConfig),
	}

	storageSettings.Wait()
	Expect(preferences).To(Equal(
		backendconfig.StoragePreferences{
			ProcErrors:   true,
			GatewayDumps: false,
		},
	))

	preferences, err = fileUploaderProvider.GetStoragePreferences("testWorkspaceId-0")
	Expect(err).To(HaveOccurred())
	Expect(preferences).To(BeEquivalentTo(backendconfig.StoragePreferences{}))

	preferences, err = fileUploaderProvider.GetStoragePreferences("testWorkspaceId-2")
	Expect(err).To(BeNil())
	Expect(preferences).To(BeEquivalentTo(backendconfig.StoragePreferences{}))
}

func TestFileUploaderWithoutConfigUpdates(t *testing.T) {
	RegisterTestingT(t)
	ctrl := gomock.NewController(t)
	config := mock_backendconfig.NewMockBackendConfig(ctrl)

	configCh := make(chan pubsub.DataEvent)

	var ready sync.WaitGroup
	ready.Add(1)

	config.EXPECT().Subscribe(
		gomock.Any(),
		gomock.Eq(backendconfig.TopicBackendConfig),
	).DoAndReturn(func(ctx context.Context, topic backendconfig.Topic) pubsub.DataChannel {
		ready.Done()
		close(configCh)
		return configCh
	})

	p := NewProvider(context.Background(), config)
	_, err := p.GetStoragePreferences("testWorkspaceId-1")
	Expect(err).To(HaveOccurred())
}

func TestStaticProvider(t *testing.T) {
	RegisterTestingT(t)
	prefs := backendconfig.StoragePreferences{
		ProcErrors:       false,
		GatewayDumps:     true,
		ProcErrorDumps:   true,
		BatchRouterDumps: false,
		RouterDumps:      true,
	}

	storageSettings := map[string]StorageSettings{
		"testWorkspaceId-1": {
			Bucket: backendconfig.StorageBucket{
				Type:   "S3",
				Config: map[string]interface{}{},
			},
			Preferences: prefs,
		},
	}
	p := NewStaticProvider(storageSettings)

	prefs, err := p.GetStoragePreferences("testWorkspaceId-1")
	Expect(err).To(BeNil())
	Expect(prefs).To(BeEquivalentTo(prefs))

	_, err = p.GetFileManager("testWorkspaceId-1")
	Expect(err).To(BeNil())
}

func TestDefaultProvider(t *testing.T) {
	RegisterTestingT(t)
	d := NewDefaultProvider()

	prefs, err := d.GetStoragePreferences("")
	Expect(err).To(BeNil())
	Expect(prefs).To(BeEquivalentTo(backendconfig.StoragePreferences{
		ProcErrors:       true,
		GatewayDumps:     true,
		ProcErrorDumps:   true,
		BatchRouterDumps: true,
		RouterDumps:      true,
	}))

	_, err = d.GetFileManager("")
	Expect(err).To(BeNil())
}

func TestOverride(t *testing.T) {
	RegisterTestingT(t)
	config := map[string]interface{}{
		"a":          "1",
		"b":          "2",
		"c":          "3",
		"externalId": "externalId",
	}
	settings := backendconfig.StorageBucket{
		Type: "S3",
		Config: map[string]interface{}{
			"b": "4",
			"d": "5",
		},
	}
	bucket := overrideWithSettings(config, settings, "wrk-1")
	Expect(bucket.Config).To(Equal(map[string]interface{}{
		"a":          "1",
		"b":          "4",
		"c":          "3",
		"d":          "5",
		"externalId": "wrk-1",
	}))
}
