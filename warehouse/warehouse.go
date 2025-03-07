package warehouse

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bugsnag/bugsnag-go/v2"
	"github.com/cenkalti/backoff/v4"
	"github.com/lib/pq"
	"github.com/thoas/go-funk"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-server/app"
	"github.com/rudderlabs/rudder-server/config"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/info"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/services/controlplane"
	"github.com/rudderlabs/rudder-server/services/db"
	destinationdebugger "github.com/rudderlabs/rudder-server/services/debugger/destination"
	"github.com/rudderlabs/rudder-server/services/filemanager"
	"github.com/rudderlabs/rudder-server/services/pgnotifier"
	migrator "github.com/rudderlabs/rudder-server/services/sql-migrator"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/services/validators"
	"github.com/rudderlabs/rudder-server/utils/httputil"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/utils/timeutil"
	"github.com/rudderlabs/rudder-server/utils/types"
	"github.com/rudderlabs/rudder-server/warehouse/archive"
	cpclient "github.com/rudderlabs/rudder-server/warehouse/client/controlplane"
	"github.com/rudderlabs/rudder-server/warehouse/deltalake"
	"github.com/rudderlabs/rudder-server/warehouse/internal/api"
	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	"github.com/rudderlabs/rudder-server/warehouse/internal/repo"
	"github.com/rudderlabs/rudder-server/warehouse/jobs"
	"github.com/rudderlabs/rudder-server/warehouse/manager"
	"github.com/rudderlabs/rudder-server/warehouse/multitenant"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
	"github.com/rudderlabs/rudder-server/warehouse/validations"
)

var (
	application                         app.App
	webPort                             int
	dbHandle                            *sql.DB
	notifier                            pgnotifier.PgNotifierT
	tenantManager                       *multitenant.Manager
	controlPlaneClient                  *controlplane.Client
	noOfSlaveWorkerRoutines             int
	uploadFreqInS                       int64
	stagingFilesSchemaPaginationSize    int
	mainLoopSleep                       time.Duration
	stagingFilesBatchSize               int
	crashRecoverWarehouses              []string
	inRecoveryMap                       map[string]bool
	lastProcessedMarkerMap              map[string]int64
	lastProcessedMarkerMapLock          sync.RWMutex
	warehouseMode                       string
	warehouseSyncPreFetchCount          int
	warehouseSyncFreqIgnore             bool
	minRetryAttempts                    int
	retryTimeWindow                     time.Duration
	maxStagingFileReadBufferCapacityInK int
	connectionsMap                      map[string]map[string]warehouseutils.Warehouse // destID -> sourceID -> warehouse map
	connectionsMapLock                  sync.RWMutex
	triggerUploadsMap                   map[string]bool // `whType:sourceID:destinationID` -> boolean value representing if an upload was triggered or not
	triggerUploadsMapLock               sync.RWMutex
	sourceIDsByWorkspace                map[string][]string // workspaceID -> []sourceIDs
	sourceIDsByWorkspaceLock            sync.RWMutex
	longRunningUploadStatThresholdInMin time.Duration
	pkgLogger                           logger.Logger
	numLoadFileUploadWorkers            int
	slaveUploadTimeout                  time.Duration
	tableCountQueryTimeout              time.Duration
	runningMode                         string
	uploadStatusTrackFrequency          time.Duration
	uploadAllocatorSleep                time.Duration
	waitForConfig                       time.Duration
	waitForWorkerSleep                  time.Duration
	uploadBufferTimeInMin               int
	ShouldForceSetLowerVersion          bool
	skipDeepEqualSchemas                bool
	maxParallelJobCreation              int
	enableJitterForSyncs                bool
	asyncWh                             *jobs.AsyncJobWhT
	configBackendURL                    string
	enableTunnelling                    bool
)

var (
	host, user, password, dbname, sslMode, appName string
	port                                           int
)

// warehouses worker modes
const (
	MasterMode         = "master"
	SlaveMode          = "slave"
	MasterSlaveMode    = "master_and_slave"
	EmbeddedMode       = "embedded"
	EmbeddedMasterMode = "embedded_master"
)

const (
	DegradedMode        = "degraded"
	triggerUploadQPName = "triggerUpload"
)

type (
	WorkerIdentifierT string
	JobIDT            int64
)

type HandleT struct {
	destType                          string
	warehouses                        []warehouseutils.Warehouse
	dbHandle                          *sql.DB
	warehouseDBHandle                 *DB
	stagingRepo                       *repo.StagingFiles
	notifier                          pgnotifier.PgNotifierT
	isEnabled                         bool
	configSubscriberLock              sync.RWMutex
	workerChannelMap                  map[string]chan *UploadJobT
	workerChannelMapLock              sync.RWMutex
	initialConfigFetched              bool
	inProgressMap                     map[WorkerIdentifierT][]JobIDT
	inProgressMapLock                 sync.RWMutex
	areBeingEnqueuedLock              sync.RWMutex
	noOfWorkers                       int
	activeWorkerCount                 int
	activeWorkerCountLock             sync.RWMutex
	maxConcurrentUploadJobs           int
	allowMultipleSourcesForJobsPickup bool
	workspaceBySourceIDs              map[string]string
	workspaceBySourceIDsLock          sync.RWMutex
	tenantManager                     multitenant.Manager
	stats                             stats.Stats
	Now                               string
	cpInternalClient                  cpclient.InternalControlPlane

	backgroundCancel context.CancelFunc
	backgroundGroup  errgroup.Group
	backgroundWait   func() error
}

type ErrorResponseT struct {
	Error string
}

func Init4() {
	loadConfig()
	pkgLogger = logger.NewLogger().Child("warehouse")
}

func loadConfig() {
	// Port where WH is running
	config.RegisterIntConfigVariable(8082, &webPort, false, 1, "Warehouse.webPort")
	config.RegisterIntConfigVariable(4, &noOfSlaveWorkerRoutines, true, 1, "Warehouse.noOfSlaveWorkerRoutines")
	config.RegisterIntConfigVariable(960, &stagingFilesBatchSize, true, 1, "Warehouse.stagingFilesBatchSize")
	config.RegisterInt64ConfigVariable(1800, &uploadFreqInS, true, 1, "Warehouse.uploadFreqInS")
	config.RegisterDurationConfigVariable(5, &mainLoopSleep, true, time.Second, []string{"Warehouse.mainLoopSleep", "Warehouse.mainLoopSleepInS"}...)
	crashRecoverWarehouses = []string{warehouseutils.RS, warehouseutils.POSTGRES, warehouseutils.MSSQL, warehouseutils.AZURE_SYNAPSE, warehouseutils.DELTALAKE}
	inRecoveryMap = map[string]bool{}
	lastProcessedMarkerMap = map[string]int64{}
	config.RegisterStringConfigVariable("embedded", &warehouseMode, false, "Warehouse.mode")
	host = config.GetString("WAREHOUSE_JOBS_DB_HOST", "localhost")
	user = config.GetString("WAREHOUSE_JOBS_DB_USER", "ubuntu")
	dbname = config.GetString("WAREHOUSE_JOBS_DB_DB_NAME", "ubuntu")
	port = config.GetInt("WAREHOUSE_JOBS_DB_PORT", 5432)
	password = config.GetString("WAREHOUSE_JOBS_DB_PASSWORD", "ubuntu") // Reading secrets from
	sslMode = config.GetString("WAREHOUSE_JOBS_DB_SSL_MODE", "disable")
	configBackendURL = config.GetString("CONFIG_BACKEND_URL", "api.rudderlabs.com")
	enableTunnelling = config.GetBool("ENABLE_TUNNELLING", true)
	config.RegisterIntConfigVariable(10, &warehouseSyncPreFetchCount, true, 1, "Warehouse.warehouseSyncPreFetchCount")
	config.RegisterIntConfigVariable(100, &stagingFilesSchemaPaginationSize, true, 1, "Warehouse.stagingFilesSchemaPaginationSize")
	config.RegisterBoolConfigVariable(false, &warehouseSyncFreqIgnore, true, "Warehouse.warehouseSyncFreqIgnore")
	config.RegisterIntConfigVariable(3, &minRetryAttempts, true, 1, "Warehouse.minRetryAttempts")
	config.RegisterDurationConfigVariable(180, &retryTimeWindow, true, time.Minute, []string{"Warehouse.retryTimeWindow", "Warehouse.retryTimeWindowInMins"}...)
	connectionsMap = map[string]map[string]warehouseutils.Warehouse{}
	triggerUploadsMap = map[string]bool{}
	sourceIDsByWorkspace = map[string][]string{}
	config.RegisterIntConfigVariable(10240, &maxStagingFileReadBufferCapacityInK, true, 1, "Warehouse.maxStagingFileReadBufferCapacityInK")
	config.RegisterDurationConfigVariable(120, &longRunningUploadStatThresholdInMin, true, time.Minute, []string{"Warehouse.longRunningUploadStatThreshold", "Warehouse.longRunningUploadStatThresholdInMin"}...)
	config.RegisterDurationConfigVariable(10, &slaveUploadTimeout, true, time.Minute, []string{"Warehouse.slaveUploadTimeout", "Warehouse.slaveUploadTimeoutInMin"}...)
	config.RegisterIntConfigVariable(8, &numLoadFileUploadWorkers, true, 1, "Warehouse.numLoadFileUploadWorkers")
	runningMode = config.GetString("Warehouse.runningMode", "")
	config.RegisterDurationConfigVariable(30, &uploadStatusTrackFrequency, false, time.Minute, []string{"Warehouse.uploadStatusTrackFrequency", "Warehouse.uploadStatusTrackFrequencyInMin"}...)
	config.RegisterIntConfigVariable(180, &uploadBufferTimeInMin, false, 1, "Warehouse.uploadBufferTimeInMin")
	config.RegisterDurationConfigVariable(5, &uploadAllocatorSleep, false, time.Second, []string{"Warehouse.uploadAllocatorSleep", "Warehouse.uploadAllocatorSleepInS"}...)
	config.RegisterDurationConfigVariable(5, &waitForConfig, false, time.Second, []string{"Warehouse.waitForConfig", "Warehouse.waitForConfigInS"}...)
	config.RegisterDurationConfigVariable(5, &waitForWorkerSleep, false, time.Second, []string{"Warehouse.waitForWorkerSleep", "Warehouse.waitForWorkerSleepInS"}...)
	config.RegisterBoolConfigVariable(true, &ShouldForceSetLowerVersion, false, "SQLMigrator.forceSetLowerVersion")
	config.RegisterBoolConfigVariable(false, &skipDeepEqualSchemas, true, "Warehouse.skipDeepEqualSchemas")
	config.RegisterIntConfigVariable(8, &maxParallelJobCreation, true, 1, "Warehouse.maxParallelJobCreation")
	config.RegisterBoolConfigVariable(false, &enableJitterForSyncs, true, "Warehouse.enableJitterForSyncs")
	config.RegisterDurationConfigVariable(30, &tableCountQueryTimeout, true, time.Second, []string{"Warehouse.tableCountQueryTimeout", "Warehouse.tableCountQueryTimeoutInS"}...)

	appName = misc.DefaultString("rudder-server").OnError(os.Hostname())
}

// get name of the worker (`destID_namespace`) to be stored in map wh.workerChannelMap
func (wh *HandleT) workerIdentifier(warehouse warehouseutils.Warehouse) (identifier string) {
	identifier = fmt.Sprintf(`%s_%s`, warehouse.Destination.ID, warehouse.Namespace)

	if wh.allowMultipleSourcesForJobsPickup {
		identifier = fmt.Sprintf(`%s_%s_%s`, warehouse.Source.ID, warehouse.Destination.ID, warehouse.Namespace)
	}
	return
}

func getDestinationFromConnectionMap(DestinationId, SourceId string) (warehouseutils.Warehouse, error) {
	if DestinationId == "" || SourceId == "" {
		return warehouseutils.Warehouse{}, errors.New("invalid Parameters")
	}
	sourceMap, ok := connectionsMap[DestinationId]
	if !ok {
		return warehouseutils.Warehouse{}, errors.New("invalid Destination Id")
	}

	conn, ok := sourceMap[SourceId]
	if !ok {
		return warehouseutils.Warehouse{}, errors.New("invalid Source Id")
	}

	return conn, nil
}

func (wh *HandleT) getActiveWorkerCount() int {
	wh.activeWorkerCountLock.Lock()
	defer wh.activeWorkerCountLock.Unlock()
	return wh.activeWorkerCount
}

func (wh *HandleT) decrementActiveWorkers() {
	// decrement number of workers actively engaged
	wh.activeWorkerCountLock.Lock()
	wh.activeWorkerCount--
	wh.activeWorkerCountLock.Unlock()
}

func (wh *HandleT) incrementActiveWorkers() {
	// increment number of workers actively engaged
	wh.activeWorkerCountLock.Lock()
	wh.activeWorkerCount++
	wh.activeWorkerCountLock.Unlock()
}

func (wh *HandleT) initWorker() chan *UploadJobT {
	workerChan := make(chan *UploadJobT, 1000)
	for i := 0; i < wh.maxConcurrentUploadJobs; i++ {
		wh.backgroundGroup.Go(func() error {
			for uploadJob := range workerChan {
				wh.incrementActiveWorkers()
				err := wh.handleUploadJob(uploadJob)
				if err != nil {
					pkgLogger.Errorf("[WH] Failed in handle Upload jobs for worker: %+w", err)
				}
				wh.removeDestInProgress(uploadJob.warehouse, uploadJob.upload.ID)
				wh.decrementActiveWorkers()
			}
			return nil
		})
	}
	return workerChan
}

func (*HandleT) handleUploadJob(uploadJob *UploadJobT) (err error) {
	// Process the upload job
	err = uploadJob.run()
	return
}

// Backend Config subscriber subscribes to backend-config and gets all the configurations that includes all sources, destinations and their latest values.
func (wh *HandleT) backendConfigSubscriber(ctx context.Context) {
	for config := range wh.tenantManager.WatchConfig(ctx) {
		wh.configSubscriberLock.Lock()
		wh.warehouses = []warehouseutils.Warehouse{}
		sourceIDsByWorkspaceLock.Lock()
		sourceIDsByWorkspace = map[string][]string{}

		wh.workspaceBySourceIDsLock.Lock()
		wh.workspaceBySourceIDs = map[string]string{}

		pkgLogger.Info(`Received updated workspace config`)
		for workspaceID, wConfig := range config {
			for _, source := range wConfig.Sources {
				if _, ok := sourceIDsByWorkspace[workspaceID]; !ok {
					sourceIDsByWorkspace[workspaceID] = []string{}
				}

				sourceIDsByWorkspace[workspaceID] = append(sourceIDsByWorkspace[workspaceID], source.ID)
				wh.workspaceBySourceIDs[source.ID] = workspaceID

				if len(source.Destinations) == 0 {
					continue
				}

				for _, destination := range source.Destinations {

					if destination.DestinationDefinition.Name != wh.destType {
						continue
					}

					if enableTunnelling {
						destination = wh.attachSSHTunnellingInfo(ctx, destination)
					}

					namespace := wh.getNamespace(destination.Config, source, destination, wh.destType)
					warehouse := warehouseutils.Warehouse{
						WorkspaceID: workspaceID,
						Source:      source,
						Destination: destination,
						Namespace:   namespace,
						Type:        wh.destType,
						Identifier:  warehouseutils.GetWarehouseIdentifier(wh.destType, source.ID, destination.ID),
					}
					wh.warehouses = append(wh.warehouses, warehouse)

					workerName := wh.workerIdentifier(warehouse)
					wh.workerChannelMapLock.Lock()
					// spawn one worker for each unique destID_namespace
					// check this commit to https://github.com/rudderlabs/rudder-server/pull/476/commits/fbfddf167aa9fc63485fe006d34e6881f5019667
					// to avoid creating goroutine for disabled sources/destinations
					if _, ok := wh.workerChannelMap[workerName]; !ok {
						workerChan := wh.initWorker()
						wh.workerChannelMap[workerName] = workerChan
					}
					wh.workerChannelMapLock.Unlock()

					connectionsMapLock.Lock()
					if connectionsMap[destination.ID] == nil {
						connectionsMap[destination.ID] = map[string]warehouseutils.Warehouse{}
					}
					if warehouse.Destination.Config["sslMode"] == "verify-ca" {
						if err := warehouseutils.WriteSSLKeys(warehouse.Destination); err.IsError() {
							pkgLogger.Error(err.Error())
							persistSSLFileErrorStat(workspaceID, wh.destType, destination.Name, destination.ID, source.Name, source.ID, err.GetErrTag())
						}
					}
					connectionsMap[destination.ID][source.ID] = warehouse
					connectionsMapLock.Unlock()

					if warehouseutils.IDResolutionEnabled() && misc.Contains(warehouseutils.IdentityEnabledWarehouses, warehouse.Type) {
						wh.setupIdentityTables(warehouse)
						if shouldPopulateHistoricIdentities && warehouse.Destination.Enabled {
							// non-blocking populate historic identities
							wh.populateHistoricIdentities(warehouse)
						}
					}
				}
			}
		}

		pkgLogger.Infof("Releasing config subscriber lock: %s", wh.destType)
		wh.workspaceBySourceIDsLock.Unlock()
		sourceIDsByWorkspaceLock.Unlock()
		wh.configSubscriberLock.Unlock()
		wh.initialConfigFetched = true
	}
}

func (wh *HandleT) attachSSHTunnellingInfo(
	ctx context.Context,
	upstream backendconfig.DestinationT,
) backendconfig.DestinationT {
	// at destination level, do we have tunnelling enabled.
	if tunnelEnabled := warehouseutils.ReadAsBool("useSSH", upstream.Config); !tunnelEnabled {
		return upstream
	}

	pkgLogger.Debugf("Fetching ssh keys for destination: %s", upstream.ID)
	keys, err := wh.cpInternalClient.GetDestinationSSHKeys(ctx, upstream.ID)
	if err != nil {
		pkgLogger.Errorf("fetching ssh keys for destination: %s", err.Error())
		return upstream
	}

	replica := backendconfig.DestinationT{}
	if err := DeepCopy(upstream, &replica); err != nil {
		pkgLogger.Errorf("deep copying the destination: %s failed: %s", upstream.ID, err)
		return upstream
	}

	replica.Config["sshPrivateKey"] = keys.PrivateKey
	return replica
}

func DeepCopy(src, dest interface{}) error {
	byt, err := json.Marshal(src)
	if err != nil {
		return err
	}

	return json.Unmarshal(byt, dest)
}

func GetKeyAsBool(key string, conf map[string]interface{}) bool {
	if val, ok := conf[key]; ok {
		if ok := val.(bool); ok {
			return val.(bool)
		}
	}
	return false
}

// getNamespace sets namespace name in the following order
//  1. user set name from destinationConfig
//  2. from existing record in wh_schemas with same source + dest combo
//  3. convert source name
func (wh *HandleT) getNamespace(configI interface{}, source backendconfig.SourceT, destination backendconfig.DestinationT, destType string) string {
	configMap := configI.(map[string]interface{})
	var namespace string
	if destType == warehouseutils.CLICKHOUSE {
		// TODO: Handle if configMap["database"] is nil
		return configMap["database"].(string)
	}
	if configMap["namespace"] != nil {
		namespace = configMap["namespace"].(string)
		if len(strings.TrimSpace(namespace)) > 0 {
			return warehouseutils.ToProviderCase(destType, warehouseutils.ToSafeNamespace(destType, namespace))
		}
	}
	// TODO: Move config to global level based on use case
	namespacePrefix := config.GetString(fmt.Sprintf("Warehouse.%s.customDatasetPrefix", warehouseutils.WHDestNameMap[destType]), "")
	if namespacePrefix != "" {
		return warehouseutils.ToProviderCase(destType, warehouseutils.ToSafeNamespace(destType, fmt.Sprintf(`%s_%s`, namespacePrefix, source.Name)))
	}
	var exists bool
	if namespace, exists = warehouseutils.GetNamespace(source, destination, wh.dbHandle); !exists {
		namespace = warehouseutils.ToProviderCase(destType, warehouseutils.ToSafeNamespace(destType, source.Name))
	}
	return namespace
}

func (wh *HandleT) getPendingStagingFiles(ctx context.Context, warehouse warehouseutils.Warehouse) ([]*model.StagingFile, error) {
	var lastStagingFileID int64
	sqlStatement := fmt.Sprintf(`
	SELECT
	  end_staging_file_id
	FROM
	  %[1]s UT
	WHERE
	  UT.destination_type = '%[2]s'
	  AND UT.source_id = '%[3]s'
	  AND UT.destination_id = '%[4]s'
	ORDER BY
	  UT.id DESC;
`,
		warehouseutils.WarehouseUploadsTable,
		warehouse.Type,
		warehouse.Source.ID,
		warehouse.Destination.ID,
	)

	err := wh.dbHandle.QueryRow(sqlStatement).Scan(&lastStagingFileID)
	if err != nil && err != sql.ErrNoRows {
		panic(fmt.Errorf("query: %s failed with Error : %w", sqlStatement, err))
	}

	stagingFilesList, err := wh.stagingRepo.GetAfterID(
		ctx,
		warehouse.Source.ID,
		warehouse.Destination.ID,
		lastStagingFileID,
	)
	if err != nil {
		return nil, err
	}

	stagingFilesListPtr := make([]*model.StagingFile, len(stagingFilesList))
	for i := range stagingFilesList {
		stagingFilesListPtr[i] = &stagingFilesList[i]
	}

	return stagingFilesListPtr, nil
}

func (wh *HandleT) initUpload(warehouse warehouseutils.Warehouse, jsonUploadsList []*model.StagingFile, isUploadTriggered bool, priority int, uploadStartAfter time.Time) {
	sqlStatement := fmt.Sprintf(`
		INSERT INTO %s (
		  source_id, namespace, workspace_id, destination_id,
		  destination_type, start_staging_file_id,
		  end_staging_file_id, start_load_file_id,
		  end_load_file_id, status, schema,
		  error, metadata, first_event_at,
		  last_event_at, created_at, updated_at
		)
		VALUES
		  (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16, $17
		  ) RETURNING id;
`,
		warehouseutils.WarehouseUploadsTable,
	)
	pkgLogger.Infof("WH: %s: Creating record in %s table: %v", wh.destType, warehouseutils.WarehouseUploadsTable, sqlStatement)
	stmt, err := wh.dbHandle.Prepare(sqlStatement)
	if err != nil {
		panic(err)
	}
	defer stmt.Close()

	startJSONID := jsonUploadsList[0].ID
	endJSONID := jsonUploadsList[len(jsonUploadsList)-1].ID
	namespace := warehouse.Namespace

	var firstEventAt, lastEventAt time.Time
	if ok := jsonUploadsList[0].FirstEventAt.IsZero(); !ok {
		firstEventAt = jsonUploadsList[0].FirstEventAt
	}
	if ok := jsonUploadsList[len(jsonUploadsList)-1].LastEventAt.IsZero(); !ok {
		lastEventAt = jsonUploadsList[len(jsonUploadsList)-1].LastEventAt
	}

	now := timeutil.Now()
	metadataMap := map[string]interface{}{
		"use_rudder_storage": jsonUploadsList[0].UseRudderStorage, // TODO: Since the use_rudder_storage is now being populated for both the staging and load files. Let's try to leverage it instead of hard coding it from the first staging file.
		"source_batch_id":    jsonUploadsList[0].SourceBatchID,
		"source_task_id":     jsonUploadsList[0].SourceTaskID,
		"source_task_run_id": jsonUploadsList[0].SourceTaskRunID,
		"source_job_id":      jsonUploadsList[0].SourceJobID,
		"source_job_run_id":  jsonUploadsList[0].SourceJobRunID,
		"load_file_type":     warehouseutils.GetLoadFileType(wh.destType),
		"nextRetryTime":      uploadStartAfter.Format(time.RFC3339),
	}
	if isUploadTriggered {
		// set priority to 50 if the upload was manually triggered
		metadataMap["priority"] = 50
	}
	if priority != 0 {
		metadataMap["priority"] = priority
	}
	metadata, err := json.Marshal(metadataMap)
	if err != nil {
		panic(err)
	}
	row := stmt.QueryRow(
		warehouse.Source.ID,
		namespace,
		warehouse.WorkspaceID,
		warehouse.Destination.ID,
		wh.destType,
		startJSONID,
		endJSONID,
		0,
		0,
		model.Waiting,
		"{}",
		"{}",
		metadata,
		firstEventAt,
		lastEventAt,
		now,
		now,
	)

	var uploadID int64
	err = row.Scan(&uploadID)
	if err != nil {
		panic(err)
	}
}

func (wh *HandleT) setDestInProgress(warehouse warehouseutils.Warehouse, jobID int64) {
	identifier := wh.workerIdentifier(warehouse)
	wh.inProgressMapLock.Lock()
	defer wh.inProgressMapLock.Unlock()
	wh.inProgressMap[WorkerIdentifierT(identifier)] = append(wh.inProgressMap[WorkerIdentifierT(identifier)], JobIDT(jobID))
}

func (wh *HandleT) removeDestInProgress(warehouse warehouseutils.Warehouse, jobID int64) {
	wh.inProgressMapLock.Lock()
	defer wh.inProgressMapLock.Unlock()
	if idx, inProgress := wh.isUploadJobInProgress(warehouse, jobID); inProgress {
		identifier := wh.workerIdentifier(warehouse)
		wh.inProgressMap[WorkerIdentifierT(identifier)] = removeFromJobsIDT(wh.inProgressMap[WorkerIdentifierT(identifier)], idx)
	}
}

func (wh *HandleT) isUploadJobInProgress(warehouse warehouseutils.Warehouse, jobID int64) (inProgressIdx int, inProgress bool) {
	identifier := wh.workerIdentifier(warehouse)
	for idx, id := range wh.inProgressMap[WorkerIdentifierT(identifier)] {
		if jobID == int64(id) {
			inProgress = true
			inProgressIdx = idx
			return
		}
	}
	return
}

func removeFromJobsIDT(slice []JobIDT, idx int) []JobIDT {
	return append(slice[:idx], slice[idx+1:]...)
}

func getUploadFreqInS(syncFrequency string) int64 {
	freqInS := uploadFreqInS
	if syncFrequency != "" {
		freqInMin, _ := strconv.ParseInt(syncFrequency, 10, 64)
		freqInS = freqInMin * 60
	}
	return freqInS
}

func uploadFrequencyExceeded(warehouse warehouseutils.Warehouse, syncFrequency string) bool {
	freqInS := getUploadFreqInS(syncFrequency)
	lastProcessedMarkerMapLock.Lock()
	defer lastProcessedMarkerMapLock.Unlock()
	if lastExecTime, ok := lastProcessedMarkerMap[warehouse.Identifier]; ok && timeutil.Now().Unix()-lastExecTime < freqInS {
		return true
	}
	return false
}

func setLastProcessedMarker(warehouse warehouseutils.Warehouse, lastProcessedTime time.Time) {
	lastProcessedMarkerMapLock.Lock()
	defer lastProcessedMarkerMapLock.Unlock()
	lastProcessedMarkerMap[warehouse.Identifier] = lastProcessedTime.Unix()
}

func (wh *HandleT) createUploadJobsFromStagingFiles(warehouse warehouseutils.Warehouse, _ manager.ManagerI, stagingFilesList []*model.StagingFile, priority int, uploadStartAfter time.Time) {
	// count := 0
	// Process staging files in batches of stagingFilesBatchSize
	// E.g. If there are 1000 pending staging files and stagingFilesBatchSize is 100,
	// Then we create 10 new entries in wh_uploads table each with 100 staging files
	var (
		stagingFilesInUpload []*model.StagingFile
		counter              int
	)
	uploadTriggered := isUploadTriggered(warehouse)

	initUpload := func() {
		wh.initUpload(warehouse, stagingFilesInUpload, uploadTriggered, priority, uploadStartAfter)
		stagingFilesInUpload = []*model.StagingFile{}
		counter = 0
	}
	for idx, sFile := range stagingFilesList {
		if idx > 0 && counter > 0 && sFile.UseRudderStorage != stagingFilesList[idx-1].UseRudderStorage {
			initUpload()
		}

		stagingFilesInUpload = append(stagingFilesInUpload, sFile)
		counter++
		if counter == stagingFilesBatchSize || idx == len(stagingFilesList)-1 {
			initUpload()
		}
	}

	// reset upload trigger if the upload was triggered
	if uploadTriggered {
		clearTriggeredUpload(warehouse)
	}
}

func getUploadStartAfterTime() time.Time {
	if enableJitterForSyncs {
		return timeutil.Now().Add(time.Duration(rand.Intn(15)) * time.Second)
	}
	return time.Now()
}

func (wh *HandleT) getLatestUploadStatus(warehouse *warehouseutils.Warehouse) (int64, string, int) {
	uploadID, status, priority, err := wh.warehouseDBHandle.GetLatestUploadStatus(
		context.TODO(),
		warehouse.Type,
		warehouse.Source.ID,
		warehouse.Destination.ID)
	if err != nil {
		pkgLogger.Errorf(`Error getting latest upload status for warehouse: %v`, err)
	}

	return uploadID, status, priority
}

func (wh *HandleT) deleteWaitingUploadJob(jobID int64) {
	sqlStatement := fmt.Sprintf(`
		DELETE FROM %s WHERE id = %d AND status = '%s'`,
		warehouseutils.WarehouseUploadsTable,
		jobID,
		model.Waiting,
	)
	_, err := wh.dbHandle.Exec(sqlStatement)
	if err != nil {
		pkgLogger.Errorf(`Error deleting upload job: %d in waiting state: %v`, jobID, err)
	}
}

func (wh *HandleT) createJobs(ctx context.Context, warehouse warehouseutils.Warehouse) (err error) {
	whManager, err := manager.New(wh.destType)
	if err != nil {
		return err
	}

	// Step 1: Crash recovery after restart
	// Remove pending temp tables in Redshift etc.
	_, ok := inRecoveryMap[warehouse.Destination.ID]
	if ok {
		pkgLogger.Infof("[WH]: Crash recovering for %s:%s", wh.destType, warehouse.Destination.ID)
		err = whManager.CrashRecover(warehouse)
		if err != nil {
			return err
		}
		delete(inRecoveryMap, warehouse.Destination.ID)
	}

	if !wh.canCreateUpload(warehouse) {
		pkgLogger.Debugf("[WH]: Skipping upload loop since %s upload freq not exceeded", warehouse.Identifier)
		return nil
	}

	wh.areBeingEnqueuedLock.Lock()

	priority := 0
	uploadID, uploadStatus, uploadPriority := wh.getLatestUploadStatus(&warehouse)
	if uploadStatus == model.Waiting {
		// If it is present do nothing else delete it
		if _, inProgress := wh.isUploadJobInProgress(warehouse, uploadID); !inProgress {
			wh.deleteWaitingUploadJob(uploadID)
			priority = uploadPriority // copy the priority from the latest upload job.
		}
	}

	wh.areBeingEnqueuedLock.Unlock()

	stagingFilesFetchStat := wh.stats.NewTaggedStat("wh_scheduler.pending_staging_files", stats.TimerType, stats.Tags{
		"workspaceId":   warehouse.WorkspaceID,
		"destinationID": warehouse.Destination.ID,
		"destType":      warehouse.Destination.DestinationDefinition.Name,
	})
	stagingFilesFetchStat.Start()
	stagingFilesList, err := wh.getPendingStagingFiles(ctx, warehouse)
	if err != nil {
		pkgLogger.Errorf("[WH]: Failed to get pending staging files: %s with error %v", warehouse.Identifier, err)
		return err
	}
	stagingFilesFetchStat.End()

	if len(stagingFilesList) == 0 {
		pkgLogger.Debugf("[WH]: Found no pending staging files for %s", warehouse.Identifier)
		return nil
	}

	uploadJobCreationStat := wh.stats.NewTaggedStat("wh_scheduler.create_upload_jobs", stats.TimerType, stats.Tags{
		"workspaceId":   warehouse.WorkspaceID,
		"destinationID": warehouse.Destination.ID,
		"destType":      warehouse.Destination.DestinationDefinition.Name,
	})
	uploadJobCreationStat.Start()

	uploadStartAfter := getUploadStartAfterTime()
	wh.createUploadJobsFromStagingFiles(warehouse, whManager, stagingFilesList, priority, uploadStartAfter)
	setLastProcessedMarker(warehouse, uploadStartAfter)

	uploadJobCreationStat.End()

	return nil
}

func (wh *HandleT) mainLoop(ctx context.Context) {
	for {
		if !wh.isEnabled {
			select {
			case <-ctx.Done():
				return
			case <-time.After(mainLoopSleep):
			}
			continue
		}

		jobCreationChan := make(chan struct{}, maxParallelJobCreation)
		wh.configSubscriberLock.RLock()
		wg := sync.WaitGroup{}
		wg.Add(len(wh.warehouses))

		whTotalSchedulingStats := wh.stats.NewStat("wh_scheduler.total_scheduling_time", stats.TimerType)
		whTotalSchedulingStats.Start()

		for _, warehouse := range wh.warehouses {
			w := warehouse
			rruntime.GoForWarehouse(func() {
				jobCreationChan <- struct{}{}
				defer func() {
					wg.Done()
					<-jobCreationChan
				}()

				pkgLogger.Debugf("[WH] Processing Jobs for warehouse: %s", w.Identifier)
				err := wh.createJobs(ctx, w)
				if err != nil {
					pkgLogger.Errorf("[WH] Failed to process warehouse Jobs: %v", err)
				}
			})
		}
		wh.configSubscriberLock.RUnlock()
		wg.Wait()

		whTotalSchedulingStats.End()
		wh.stats.NewStat("wh_scheduler.warehouse_length", stats.CountType).Count(len(wh.warehouses)) // Correlation between number of warehouses and scheduling time.
		select {
		case <-ctx.Done():
			return
		case <-time.After(mainLoopSleep):
		}
	}
}

func (wh *HandleT) processingStats(ctx context.Context, availableWorkers int, skipIdentifiers []string, skipIdentifiersSQL string) error {
	var (
		pendingJobs             int
		query                   string
		pickupLagInSeconds      float64
		pickupWaitTimeInSeconds float64
		err                     error
		Now                     = "NOW()"
		degradedWorkspaces      = tenantManager.DegradedWorkspaces()
	)
	if wh.Now != "" {
		Now = wh.Now
	}
	if degradedWorkspaces == nil {
		degradedWorkspaces = []string{}
	}

	query = fmt.Sprintf(`
		SELECT
			COALESCE(COUNT(*), 0) AS pending_jobs,
			COALESCE(EXTRACT(EPOCH FROM(AGE(%[7]s, MIN(COALESCE(metadata->>'nextRetryTime', %[7]s::text)::timestamptz)))), 0) AS pickup_lag_in_seconds,
			COALESCE(SUM(EXTRACT(EPOCH FROM AGE(%[7]s, COALESCE(metadata->>'nextRetryTime', %[7]s::text)::timestamptz))), 0) AS pickup_wait_time_in_seconds
		FROM
			%[1]s t
		WHERE
			t.destination_type = '%[2]s' AND
			t.in_progress = %[3]t AND
			t.status != '%[4]s' AND
			t.status != '%[5]s' %[6]s AND
			COALESCE(metadata->>'nextRetryTime', %[7]s::text)::timestamptz <= %[7]s AND
			workspace_id <> ALL ($1);
`,
		warehouseutils.WarehouseUploadsTable,
		wh.destType,
		false,
		model.ExportedData,
		model.Aborted,
		skipIdentifiersSQL,
		Now,
	)

	if len(skipIdentifiers) > 0 {
		if err = wh.dbHandle.QueryRowContext(
			ctx,
			query,
			pq.Array(degradedWorkspaces),
			pq.Array(skipIdentifiers),
		).Scan(&pendingJobs, &pickupLagInSeconds, &pickupWaitTimeInSeconds); err != nil {
			return fmt.Errorf("processing  with skip identifiers: %w", err)
		}
	} else {
		if err = wh.dbHandle.QueryRowContext(
			ctx,
			query,
			pq.Array(degradedWorkspaces),
		).Scan(&pendingJobs, &pickupLagInSeconds, &pickupWaitTimeInSeconds); err != nil {
			return fmt.Errorf("count pending jobs: %w", err)
		}
	}

	pendingJobsStat := wh.stats.NewTaggedStat("wh_processing_pending_jobs", stats.CountType, stats.Tags{
		"module":   moduleName,
		"destType": wh.destType,
	})
	pendingJobsStat.Count(pendingJobs)

	availableWorkersStat := wh.stats.NewTaggedStat("wh_processing_available_workers", stats.GaugeType, stats.Tags{
		"module":   moduleName,
		"destType": wh.destType,
	})
	availableWorkersStat.Gauge(availableWorkers)

	pickupLagStat := wh.stats.NewTaggedStat("wh_processing_pickup_lag", stats.TimerType, stats.Tags{
		"module":   moduleName,
		"destType": wh.destType,
	})
	pickupLagStat.SendTiming(time.Duration(pickupLagInSeconds) * time.Second)

	pickupWaitTimeStat := wh.stats.NewTaggedStat("wh_processing_pickup_wait_time", stats.TimerType, stats.Tags{
		"module":   moduleName,
		"destType": wh.destType,
	})
	pickupWaitTimeStat.SendTiming(time.Duration(pickupWaitTimeInSeconds) * time.Second)
	return nil
}

func (wh *HandleT) getUploadsToProcess(ctx context.Context, availableWorkers int, skipIdentifiers []string) ([]*UploadJobT, error) {
	var skipIdentifiersSQL string
	partitionIdentifierSQL := `destination_id, namespace`

	if len(skipIdentifiers) > 0 {
		skipIdentifiersSQL = `AND ((destination_id || '_' || namespace)) != ALL($2)`
	}

	if wh.allowMultipleSourcesForJobsPickup {
		if len(skipIdentifiers) > 0 {
			skipIdentifiersSQL = `AND ((source_id || '_' || destination_id || '_' || namespace)) != ALL($2)`
		}
		partitionIdentifierSQL = fmt.Sprintf(`%s, %s`, "source_id", partitionIdentifierSQL)
	}

	sqlStatement := fmt.Sprintf(`
			SELECT
				id,
				status,
				schema,
				mergedSchema,
				namespace,
				workspace_id,
				source_id,
				destination_id,
				destination_type,
				start_staging_file_id,
				end_staging_file_id,
				start_load_file_id,
				end_load_file_id,
				error,
				metadata,
				timings->0 as firstTiming,
				timings->-1 as lastTiming,
				timings,
				COALESCE(metadata->>'priority', '100')::int,
				first_event_at,
				last_event_at
			FROM (
				SELECT
					ROW_NUMBER() OVER (PARTITION BY %s ORDER BY COALESCE(metadata->>'priority', '100')::int ASC, id ASC) AS row_number,
					t.*
				FROM
					%s t
				WHERE
					t.destination_type = '%s' AND
					t.in_progress=%t AND
					t.status != '%s' AND
					t.status != '%s' %s AND
					COALESCE(metadata->>'nextRetryTime', NOW()::text)::timestamptz <= NOW() AND
          			workspace_id <> ALL ($1)
			) grouped_uploads
			WHERE
				grouped_uploads.row_number = 1
			ORDER BY
				COALESCE(metadata->>'priority', '100')::int ASC,
				id ASC
			LIMIT %d;
`,
		partitionIdentifierSQL,
		warehouseutils.WarehouseUploadsTable,
		wh.destType,
		false,
		model.ExportedData,
		model.Aborted,
		skipIdentifiersSQL,
		availableWorkers,
	)

	var (
		rows               *sql.Rows
		err                error
		degradedWorkspaces = tenantManager.DegradedWorkspaces()
	)
	if degradedWorkspaces == nil {
		degradedWorkspaces = []string{}
	}

	if len(skipIdentifiers) > 0 {
		rows, err = wh.dbHandle.QueryContext(
			ctx,
			sqlStatement,
			pq.Array(degradedWorkspaces),
			pq.Array(skipIdentifiers),
		)
	} else {
		rows, err = wh.dbHandle.QueryContext(
			ctx,
			sqlStatement,
			pq.Array(degradedWorkspaces),
		)
	}

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return []*UploadJobT{}, err
	}

	if errors.Is(err, sql.ErrNoRows) {
		return []*UploadJobT{}, nil
	}
	defer rows.Close()

	var uploadJobs []*UploadJobT
	for rows.Next() {
		var (
			upload                    Upload
			schema                    json.RawMessage
			mergedSchema              json.RawMessage
			firstTiming               sql.NullString
			lastTiming                sql.NullString
			firstEventAt, lastEventAt sql.NullTime
		)

		err := rows.Scan(
			&upload.ID,
			&upload.Status,
			&schema,
			&mergedSchema,
			&upload.Namespace,
			&upload.WorkspaceID,
			&upload.SourceID,
			&upload.DestinationID,
			&upload.DestinationType,
			&upload.StartStagingFileID,
			&upload.EndStagingFileID,
			&upload.StartLoadFileID,
			&upload.EndLoadFileID,
			&upload.Error,
			&upload.Metadata,
			&firstTiming,
			&lastTiming,
			&upload.TimingsObj,
			&upload.Priority,
			&firstEventAt,
			&lastEventAt,
		)
		if err != nil {
			panic(fmt.Errorf("failed to scan result from query: %s\n with error : %w", sqlStatement, err))
		}
		upload.FirstEventAt = firstEventAt.Time
		upload.LastEventAt = lastEventAt.Time
		upload.UploadSchema = warehouseutils.JSONSchemaToMap(schema)
		upload.MergedSchema = warehouseutils.JSONSchemaToMap(mergedSchema)

		// TODO: replace gjson with jsoniter
		// cloud sources info
		upload.SourceBatchID = gjson.GetBytes(upload.Metadata, "source_batch_id").String()
		upload.SourceTaskID = gjson.GetBytes(upload.Metadata, "source_task_id").String()
		upload.SourceTaskRunID = gjson.GetBytes(upload.Metadata, "source_task_run_id").String()
		upload.SourceJobID = gjson.GetBytes(upload.Metadata, "source_job_id").String()
		upload.SourceJobRunID = gjson.GetBytes(upload.Metadata, "source_job_run_id").String()
		// load file type
		upload.LoadFileType = gjson.GetBytes(upload.Metadata, "load_file_type").String()

		_, upload.FirstAttemptAt = warehouseutils.TimingFromJSONString(firstTiming)
		var lastStatus string
		lastStatus, upload.LastAttemptAt = warehouseutils.TimingFromJSONString(lastTiming)
		upload.Attempts = gjson.Get(string(upload.Error), fmt.Sprintf(`%s.attempt`, lastStatus)).Int()

		if upload.WorkspaceID == "" {
			var ok bool
			wh.workspaceBySourceIDsLock.Lock()
			upload.WorkspaceID, ok = wh.workspaceBySourceIDs[upload.SourceID]
			wh.workspaceBySourceIDsLock.Unlock()

			if !ok {
				pkgLogger.Warnf("could not find workspace id for source id: %s", upload.SourceID)
			}
		}

		wh.configSubscriberLock.RLock()
		warehouse, ok := funk.Find(wh.warehouses, func(w warehouseutils.Warehouse) bool {
			return w.Source.ID == upload.SourceID && w.Destination.ID == upload.DestinationID
		}).(warehouseutils.Warehouse)
		wh.configSubscriberLock.RUnlock()

		upload.UseRudderStorage = warehouse.GetBoolDestinationConfig("useRudderStorage")

		if !ok {
			uploadJob := UploadJobT{
				upload:   &upload,
				dbHandle: wh.dbHandle,
				stats:    wh.stats,
			}
			err := fmt.Errorf("unable to find source : %s or destination : %s, both or the connection between them", upload.SourceID, upload.DestinationID)
			_, _ = uploadJob.setUploadError(err, model.Aborted)
			pkgLogger.Errorf("%v", err)
			continue
		}

		upload.SourceType = warehouse.Source.SourceDefinition.Name
		upload.SourceCategory = warehouse.Source.SourceDefinition.Category

		stagingFilesList, err := wh.stagingRepo.GetInRange(
			ctx,
			warehouse.Source.ID,
			warehouse.Destination.ID,
			upload.StartStagingFileID,
			upload.EndStagingFileID,
		)
		if err != nil {
			return nil, err
		}

		stagingFileIDs := make([]int64, len(stagingFilesList))
		stagingFileListPtr := make([]*model.StagingFile, len(stagingFilesList))
		for i := range stagingFilesList {
			stagingFileIDs[i] = stagingFilesList[i].ID
			stagingFileListPtr[i] = &stagingFilesList[i]
		}

		whManager, err := manager.New(wh.destType)
		if err != nil {
			return nil, err
		}

		uploadJob := UploadJobT{
			upload:               &upload,
			stagingFiles:         stagingFileListPtr,
			stagingFileIDs:       stagingFileIDs,
			warehouse:            warehouse,
			whManager:            whManager,
			dbHandle:             wh.dbHandle,
			pgNotifier:           &wh.notifier,
			destinationValidator: validations.NewDestinationValidator(),
			stats:                wh.stats,
		}

		uploadJobs = append(uploadJobs, &uploadJob)
	}

	if err = wh.processingStats(ctx, availableWorkers, skipIdentifiers, skipIdentifiersSQL); err != nil {
		return nil, fmt.Errorf("processing stats: %w", err)
	}

	return uploadJobs, nil
}

func (wh *HandleT) getInProgressNamespaces() (identifiers []string) {
	wh.inProgressMapLock.Lock()
	defer wh.inProgressMapLock.Unlock()
	for k, v := range wh.inProgressMap {
		if len(v) >= wh.maxConcurrentUploadJobs {
			identifiers = append(identifiers, string(k))
		}
	}
	return
}

func (wh *HandleT) runUploadJobAllocator(ctx context.Context) {
loop:
	for {
		if !wh.initialConfigFetched {
			select {
			case <-ctx.Done():
				break loop
			case <-time.After(waitForConfig):
			}
			continue
		}

		availableWorkers := wh.noOfWorkers - wh.getActiveWorkerCount()
		if availableWorkers < 1 {
			select {
			case <-ctx.Done():
				break loop
			case <-time.After(waitForWorkerSleep):
			}
			continue
		}

		wh.areBeingEnqueuedLock.Lock()

		inProgressNamespaces := wh.getInProgressNamespaces()
		pkgLogger.Debugf(`Current inProgress namespace identifiers for %s: %v`, wh.destType, inProgressNamespaces)

		uploadJobsToProcess, err := wh.getUploadsToProcess(ctx, availableWorkers, inProgressNamespaces)
		if err != nil {
			pkgLogger.Errorf(`Error executing getUploadsToProcess: %v`, err)
			panic(err)
		}

		for _, uploadJob := range uploadJobsToProcess {
			wh.setDestInProgress(uploadJob.warehouse, uploadJob.upload.ID)
		}
		wh.areBeingEnqueuedLock.Unlock()

		for _, uploadJob := range uploadJobsToProcess {
			workerName := wh.workerIdentifier(uploadJob.warehouse)
			wh.workerChannelMapLock.Lock()
			wh.workerChannelMap[workerName] <- uploadJob
			wh.workerChannelMapLock.Unlock()
		}

		select {
		case <-ctx.Done():
			break loop
		case <-time.After(uploadAllocatorSleep):
		}
	}

	wh.workerChannelMapLock.Lock()
	for _, workerChannel := range wh.workerChannelMap {
		close(workerChannel)
	}
	wh.workerChannelMapLock.Unlock()
}

func (wh *HandleT) uploadStatusTrack(ctx context.Context) {
	for {
		for _, warehouse := range wh.warehouses {
			source := warehouse.Source
			destination := warehouse.Destination

			if !source.Enabled || !destination.Enabled {
				continue
			}

			config := destination.Config
			// Default frequency
			syncFrequency := "1440"
			if config[warehouseutils.SyncFrequency] != nil {
				syncFrequency, _ = config[warehouseutils.SyncFrequency].(string)
			}

			timeWindow := uploadBufferTimeInMin
			if value, err := strconv.Atoi(syncFrequency); err == nil {
				timeWindow += value
			}

			sqlStatement := fmt.Sprintf(`
				select
				  created_at
				from
				  %[1]s
				where
				  source_id = '%[2]s'
				  and destination_id = '%[3]s'
				  and created_at > NOW() - interval '%[4]d MIN'
				  and created_at < NOW() - interval '%[5]d MIN'
				order by
				  created_at desc
				limit
				  1;
`,

				warehouseutils.WarehouseStagingFilesTable,
				source.ID,
				destination.ID,
				2*timeWindow,
				timeWindow,
			)

			var createdAt sql.NullTime
			err := wh.dbHandle.QueryRow(sqlStatement).Scan(&createdAt)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil && err != sql.ErrNoRows {
				panic(fmt.Errorf("Query: %s\nfailed with Error : %w", sqlStatement, err))
			}

			if !createdAt.Valid {
				continue
			}

			sqlStatement = fmt.Sprintf(`
				SELECT
				  EXISTS (
					SELECT
					  1
					FROM
					  %s
					WHERE
					  source_id = $1
					  AND destination_id = $2
					  AND (
						status = $3
						OR status = $4
						OR status LIKE $5
					  )
					  AND updated_at > $6
				  );
`,
				warehouseutils.WarehouseUploadsTable,
			)
			sqlStatementArgs := []interface{}{
				source.ID,
				destination.ID,
				model.ExportedData,
				model.Aborted,
				"%_failed",
				createdAt.Time.Format(misc.RFC3339Milli),
			}
			var (
				exists   bool
				uploaded int
			)
			err = wh.dbHandle.QueryRow(sqlStatement, sqlStatementArgs...).Scan(&exists)
			if err != nil && err != sql.ErrNoRows {
				panic(fmt.Errorf("Query: %s\nfailed with Error : %w", sqlStatement, err))
			}
			if exists {
				uploaded = 1
			}

			getUploadStatusStat("warehouse_successful_upload_exists", warehouse).Count(uploaded)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(uploadStatusTrackFrequency):
		}
	}
}

func getBucketFolder(batchID, tableName string) string {
	return fmt.Sprintf(`%v-%v`, batchID, tableName)
}

// Enable enables a router :)
func (wh *HandleT) Enable() {
	wh.isEnabled = true
}

// Disable disables a router:)
func (wh *HandleT) Disable() {
	wh.isEnabled = false
}

func (wh *HandleT) setInterruptedDestinations() {
	if !misc.Contains(crashRecoverWarehouses, wh.destType) {
		return
	}
	sqlStatement := fmt.Sprintf(`
		SELECT
		  destination_id
		FROM
		  %s
		WHERE
		  destination_type = '%s'
		  AND (
			status = '%s'
			OR status = '%s'
		  )
		  and in_progress = %t;
`,
		warehouseutils.WarehouseUploadsTable,
		wh.destType,
		getInProgressState(model.ExportedData),
		getFailedState(model.ExportedData),
		true,
	)
	rows, err := wh.dbHandle.Query(sqlStatement)
	if err != nil {
		panic(fmt.Errorf("query: %s failed with Error : %w", sqlStatement, err))
	}
	defer rows.Close()

	for rows.Next() {
		var destID string
		err := rows.Scan(&destID)
		if err != nil {
			panic(fmt.Errorf("failed to scan result from query: %s\nwith error : %w", sqlStatement, err))
		}
		inRecoveryMap[destID] = true
	}
}

func (wh *HandleT) Setup(whType string) {
	pkgLogger.Infof("WH: Warehouse Router started: %s", whType)
	wh.dbHandle = dbHandle
	// We now have access to the warehouseDBHandle through
	// which we will be running the db calls.
	wh.warehouseDBHandle = NewWarehouseDB(dbHandle)
	wh.stagingRepo = &repo.StagingFiles{
		DB: dbHandle,
	}
	wh.notifier = notifier
	wh.destType = whType
	wh.setInterruptedDestinations()
	wh.resetInProgressJobs()
	wh.Enable()
	wh.workerChannelMap = make(map[string]chan *UploadJobT)
	wh.inProgressMap = make(map[WorkerIdentifierT][]JobIDT)
	wh.tenantManager = multitenant.Manager{
		BackendConfig: backendconfig.DefaultBackendConfig,
	}
	wh.stats = stats.Default

	whName := warehouseutils.WHDestNameMap[whType]
	config.RegisterIntConfigVariable(8, &wh.noOfWorkers, true, 1, fmt.Sprintf(`Warehouse.%v.noOfWorkers`, whName), "Warehouse.noOfWorkers")
	config.RegisterIntConfigVariable(1, &wh.maxConcurrentUploadJobs, false, 1, fmt.Sprintf(`Warehouse.%v.maxConcurrentUploadJobs`, whName))
	config.RegisterBoolConfigVariable(false, &wh.allowMultipleSourcesForJobsPickup, false, fmt.Sprintf(`Warehouse.%v.allowMultipleSourcesForJobsPickup`, whName))

	wh.cpInternalClient = cpclient.NewInternalClientWithCache(
		configBackendURL,
		cpclient.BasicAuth{
			Username: config.GetString("CP_INTERNAL_API_USERNAME", ""),
			Password: config.GetString("CP_INTERNAL_API_PASSWORD", ""),
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	wh.backgroundCancel = cancel
	wh.backgroundWait = g.Wait

	g.Go(misc.WithBugsnagForWarehouse(func() error {
		wh.tenantManager.Run(ctx)
		return nil
	}))

	g.Go(misc.WithBugsnagForWarehouse(func() error {
		wh.backendConfigSubscriber(ctx)
		return nil
	}))

	g.Go(misc.WithBugsnagForWarehouse(func() error {
		wh.runUploadJobAllocator(ctx)
		return nil
	}))
	g.Go(misc.WithBugsnagForWarehouse(func() error {
		wh.mainLoop(ctx)
		return nil
	}))

	g.Go(misc.WithBugsnagForWarehouse(func() error {
		pkgLogger.Infof("WH: Warehouse Idle upload tracker started")
		wh.uploadStatusTrack(ctx)
		return nil
	}))
}

func (wh *HandleT) Shutdown() {
	wh.backgroundCancel()
	wh.backgroundWait()
}

func (wh *HandleT) resetInProgressJobs() {
	sqlStatement := fmt.Sprintf(`
		UPDATE
		  %s
		SET
		  in_progress = %t
		WHERE
		  destination_type = '%s'
		  AND in_progress = %t;
`,
		warehouseutils.WarehouseUploadsTable,
		false,
		wh.destType,
		true,
	)
	_, err := wh.dbHandle.Query(sqlStatement)
	if err != nil {
		panic(fmt.Errorf("query: %s failed with Error : %w", sqlStatement, err))
	}
}

func minimalConfigSubscriber() {
	ch := backendconfig.DefaultBackendConfig.Subscribe(context.TODO(), backendconfig.TopicBackendConfig)
	for data := range ch {
		pkgLogger.Debug("Got config from config-backend", data)
		config := data.Data.(map[string]backendconfig.ConfigT)

		sourceIDsByWorkspaceLock.Lock()
		sourceIDsByWorkspace = map[string][]string{}

		var connectionFlags backendconfig.ConnectionFlags
		for workspaceID, wConfig := range config {
			connectionFlags = wConfig.ConnectionFlags // the last connection flags should be enough, since they are all the same in multi-workspace environments
			for _, source := range wConfig.Sources {
				if _, ok := sourceIDsByWorkspace[workspaceID]; !ok {
					sourceIDsByWorkspace[workspaceID] = []string{}
				}
				sourceIDsByWorkspace[workspaceID] = append(sourceIDsByWorkspace[workspaceID], source.ID)
				for _, destination := range source.Destinations {
					if misc.Contains(warehouseutils.WarehouseDestinations, destination.DestinationDefinition.Name) {
						wh := &HandleT{
							dbHandle: dbHandle,
							destType: destination.DestinationDefinition.Name,
						}
						namespace := wh.getNamespace(destination.Config, source, destination, wh.destType)
						connectionsMapLock.Lock()
						if connectionsMap[destination.ID] == nil {
							connectionsMap[destination.ID] = map[string]warehouseutils.Warehouse{}
						}
						connectionsMap[destination.ID][source.ID] = warehouseutils.Warehouse{
							WorkspaceID: workspaceID,
							Destination: destination,
							Namespace:   namespace,
							Type:        wh.destType,
							Source:      source,
							Identifier:  warehouseutils.GetWarehouseIdentifier(wh.destType, source.ID, destination.ID),
						}
						connectionsMapLock.Unlock()
					}
				}
			}
		}
		sourceIDsByWorkspaceLock.Unlock()

		if val, ok := connectionFlags.Services["warehouse"]; ok {
			if UploadAPI.connectionManager != nil {
				UploadAPI.connectionManager.Apply(connectionFlags.URL, val)
			}
		}
	}
}

// Gets the config from config backend and extracts enabled write keys
func monitorDestRouters(ctx context.Context) {
	dstToWhRouter := make(map[string]*HandleT)

	ch := tenantManager.WatchConfig(ctx)
	for config := range ch {
		onConfigDataEvent(config, dstToWhRouter)
	}

	g, _ := errgroup.WithContext(context.Background())
	for _, wh := range dstToWhRouter {
		wh := wh
		g.Go(func() error {
			wh.Shutdown()
			return nil
		})
	}
	g.Wait()
}

func onConfigDataEvent(config map[string]backendconfig.ConfigT, dstToWhRouter map[string]*HandleT) {
	pkgLogger.Debug("Got config from config-backend", config)

	enabledDestinations := make(map[string]bool)
	var connectionFlags backendconfig.ConnectionFlags
	for _, wConfig := range config {
		connectionFlags = wConfig.ConnectionFlags // the last connection flags should be enough, since they are all the same in multi-workspace environments
		for _, source := range wConfig.Sources {
			for _, destination := range source.Destinations {
				enabledDestinations[destination.DestinationDefinition.Name] = true
				if misc.Contains(warehouseutils.WarehouseDestinations, destination.DestinationDefinition.Name) {
					wh, ok := dstToWhRouter[destination.DestinationDefinition.Name]
					if !ok {
						pkgLogger.Info("Starting a new Warehouse Destination Router: ", destination.DestinationDefinition.Name)
						wh = &HandleT{}
						wh.configSubscriberLock.Lock()
						wh.Setup(destination.DestinationDefinition.Name)
						wh.configSubscriberLock.Unlock()
						dstToWhRouter[destination.DestinationDefinition.Name] = wh
					} else {
						pkgLogger.Debug("Enabling existing Destination: ", destination.DestinationDefinition.Name)
						wh.configSubscriberLock.Lock()
						wh.Enable()
						wh.configSubscriberLock.Unlock()
					}
				}
			}
		}
	}
	if val, ok := connectionFlags.Services["warehouse"]; ok {
		if UploadAPI.connectionManager != nil {
			UploadAPI.connectionManager.Apply(connectionFlags.URL, val)
		}
	}

	keys := misc.StringKeys(dstToWhRouter)
	for _, key := range keys {
		if _, ok := enabledDestinations[key]; !ok {
			if wh, ok := dstToWhRouter[key]; ok {
				pkgLogger.Info("Disabling a existing warehouse destination: ", key)
				wh.configSubscriberLock.Lock()
				wh.Disable()
				wh.configSubscriberLock.Unlock()
			}
		}
	}
}

func setupTables(dbHandle *sql.DB) error {
	m := &migrator.Migrator{
		Handle:                     dbHandle,
		MigrationsTable:            "wh_schema_migrations",
		ShouldForceSetLowerVersion: ShouldForceSetLowerVersion,
	}

	operation := func() error {
		return m.Migrate("warehouse")
	}

	backoffWithMaxRetry := backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3)
	err := backoff.RetryNotify(operation, backoffWithMaxRetry, func(err error, t time.Duration) {
		pkgLogger.Warnf("Failed to setup WH db tables: %v, retrying after %v", err, t)
	})
	if err != nil {
		return fmt.Errorf("could not run warehouse database migrations: %w", err)
	}
	return nil
}

func CheckPGHealth(dbHandle *sql.DB) bool {
	if dbHandle == nil {
		return false
	}
	rows, err := dbHandle.Query(`SELECT 'Rudder Warehouse DB Health Check'::text as message`)
	if err != nil {
		pkgLogger.Error(err)
		return false
	}
	defer rows.Close()
	return true
}

func setConfigHandler(w http.ResponseWriter, r *http.Request) {
	pkgLogger.LogRequest(r)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error reading body: %v", err)
		http.Error(w, "can't read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var kvs []warehouseutils.KeyValue
	err = json.Unmarshal(body, &kvs)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error unmarshalling body: %v", err)
		http.Error(w, "can't unmarshall body", http.StatusBadRequest)
		return
	}

	for _, kv := range kvs {
		config.Set(kv.Key, kv.Value)
	}
	w.WriteHeader(http.StatusOK)
}

func pendingEventsHandler(w http.ResponseWriter, r *http.Request) {
	// TODO : respond with errors in a common way
	pkgLogger.LogRequest(r)

	ctx := r.Context()

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error reading body: %v", err)
		http.Error(w, "can't read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// unmarshall body
	var pendingEventsReq warehouseutils.PendingEventsRequestT
	err = json.Unmarshal(body, &pendingEventsReq)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error unmarshalling body: %v", err)
		http.Error(w, "can't unmarshall body", http.StatusBadRequest)
		return
	}

	sourceID := pendingEventsReq.SourceID

	// return error if source id is empty
	if sourceID == "" {
		pkgLogger.Errorf("[WH]: pending-events:  Empty source id")
		http.Error(w, "empty source id", http.StatusBadRequest)
		return
	}

	workspaceID, err := tenantManager.SourceToWorkspace(ctx, sourceID)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error checking if source is degraded: %v", err)
		http.Error(w, "workspaceID from sourceID not found", http.StatusBadRequest)
		return
	}

	if tenantManager.DegradedWorkspace(workspaceID) {
		pkgLogger.Infof("[WH]: Workspace (id: %q) is degraded: %v", workspaceID, err)
		http.Error(w, "workspace is in degraded mode", http.StatusServiceUnavailable)
		return
	}

	pendingEvents := false
	var (
		pendingStagingFileCount int64
		pendingUploadCount      int64
	)

	// check whether there are any pending staging files or uploads for the given source id
	// get pending staging files
	pendingStagingFileCount, err = getPendingStagingFileCount(sourceID, true)
	if err != nil {
		err := fmt.Errorf("error getting pending staging file count : %v", err)
		pkgLogger.Errorf("[WH]: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filterBy := []warehouseutils.FilterBy{{Key: "source_id", Value: sourceID}}
	if pendingEventsReq.TaskRunID != "" {
		filterBy = append(filterBy, warehouseutils.FilterBy{Key: "metadata->>'source_task_run_id'", Value: pendingEventsReq.TaskRunID})
	}

	pendingUploadCount, err = getPendingUploadCount(filterBy...)
	if err != nil {
		err := fmt.Errorf("error getting pending uploads : %v", err)
		pkgLogger.Errorf("[WH]: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// if there are any pending staging files or uploads, set pending events as true
	if (pendingStagingFileCount + pendingUploadCount) > int64(0) {
		pendingEvents = true
	}

	// read `triggerUpload` queryParam
	var triggerPendingUpload bool
	triggerUploadQP := r.URL.Query().Get(triggerUploadQPName)
	if triggerUploadQP != "" {
		triggerPendingUpload, _ = strconv.ParseBool(triggerUploadQP)
	}

	// trigger upload if there are pending events and triggerPendingUpload is true
	if pendingEvents && triggerPendingUpload {
		pkgLogger.Infof("[WH]: Triggering upload for all wh destinations connected to source '%s'", sourceID)
		wh := make([]warehouseutils.Warehouse, 0)

		// get all wh destinations for given source id
		connectionsMapLock.Lock()
		for _, srcMap := range connectionsMap {
			for srcID, w := range srcMap {
				if srcID == sourceID {
					wh = append(wh, w)
				}
			}
		}
		connectionsMapLock.Unlock()

		// return error if no such destinations found
		if len(wh) == 0 {
			err := fmt.Errorf("no warehouse destinations found for source id '%s'", sourceID)
			pkgLogger.Errorf("[WH]: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		for _, warehouse := range wh {
			triggerUpload(warehouse)
		}
	}

	// create and write response
	res := warehouseutils.PendingEventsResponseT{
		PendingEvents:            pendingEvents,
		PendingStagingFilesCount: pendingStagingFileCount,
		PendingUploadCount:       pendingUploadCount,
	}

	resBody, err := json.Marshal(res)
	if err != nil {
		err := fmt.Errorf("failed to marshall pending events response : %v", err)
		pkgLogger.Errorf("[WH]: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(resBody)
}

func getPendingStagingFileCount(sourceOrDestId string, isSourceId bool) (fileCount int64, err error) {
	sourceOrDestColumn := ""
	if isSourceId {
		sourceOrDestColumn = "source_id"
	} else {
		sourceOrDestColumn = "destination_id"
	}
	var lastStagingFileIDRes sql.NullInt64
	sqlStatement := fmt.Sprintf(`
		SELECT
		  MAX(end_staging_file_id)
		FROM
		  %[1]s
		WHERE
		  %[2]s = $1;
`,
		warehouseutils.WarehouseUploadsTable,
		sourceOrDestColumn,
	)
	err = dbHandle.QueryRow(sqlStatement, sourceOrDestId).Scan(&lastStagingFileIDRes)
	if err != nil && err != sql.ErrNoRows {
		err = fmt.Errorf("query: %s run failed with Error : %w", sqlStatement, err)
		return
	}
	lastStagingFileID := int64(0)
	if lastStagingFileIDRes.Valid {
		lastStagingFileID = lastStagingFileIDRes.Int64
	}

	sqlStatement = fmt.Sprintf(`
		SELECT
		  COUNT(*)
		FROM
		  %[1]s
		WHERE
		  id > %[2]v
		  AND %[3]s = $1;
`,
		warehouseutils.WarehouseStagingFilesTable,
		lastStagingFileID,
		sourceOrDestColumn,
	)
	err = dbHandle.QueryRow(sqlStatement, sourceOrDestId).Scan(&fileCount)
	if err != nil && err != sql.ErrNoRows {
		err = fmt.Errorf("query: %s run failed with Error : %w", sqlStatement, err)
		return
	}

	return fileCount, nil
}

func getPendingUploadCount(filters ...warehouseutils.FilterBy) (uploadCount int64, err error) {
	pkgLogger.Debugf("Fetching pending upload count with filters: %v", filters)

	query := fmt.Sprintf(`
		SELECT
		  COUNT(*)
		FROM
		  %[1]s
		WHERE
		  %[1]s.status NOT IN ('%[2]s', '%[3]s')
	`,
		warehouseutils.WarehouseUploadsTable,
		model.ExportedData,
		model.Aborted,
	)

	args := make([]interface{}, 0)
	for i, filter := range filters {
		query += fmt.Sprintf(" AND %s=$%d", filter.Key, i+1)
		args = append(args, filter.Value)
	}

	err = dbHandle.QueryRow(query, args...).Scan(&uploadCount)
	if err != nil && err != sql.ErrNoRows {
		err = fmt.Errorf("query: %s failed with Error : %w", query, err)
		return
	}

	return uploadCount, nil
}

func triggerUploadHandler(w http.ResponseWriter, r *http.Request) {
	// TODO : respond with errors in a common way
	pkgLogger.LogRequest(r)

	ctx := r.Context()

	// read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error reading body: %v", err)
		http.Error(w, "can't read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// unmarshall body
	var triggerUploadReq warehouseutils.TriggerUploadRequestT
	err = json.Unmarshal(body, &triggerUploadReq)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error unmarshalling body: %v", err)
		http.Error(w, "can't unmarshall body", http.StatusBadRequest)
		return
	}

	workspaceID, err := tenantManager.SourceToWorkspace(ctx, triggerUploadReq.SourceID)
	if err != nil {
		pkgLogger.Errorf("[WH]: Error checking if source is degraded: %v", err)
		http.Error(w, "workspaceID from sourceID not found", http.StatusBadRequest)
		return
	}

	if tenantManager.DegradedWorkspace(workspaceID) {
		pkgLogger.Infof("[WH]: Workspace (id: %q) is degraded: %v", workspaceID, err)
		http.Error(w, "workspace is in degraded mode", http.StatusServiceUnavailable)
		return
	}

	err = TriggerUploadHandler(triggerUploadReq.SourceID, triggerUploadReq.DestinationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func TriggerUploadHandler(sourceID, destID string) error {
	// return error if source id and dest id is empty
	if sourceID == "" && destID == "" {
		err := fmt.Errorf("empty source and destination id")
		pkgLogger.Errorf("[WH]: trigger upload : %v", err)
		return err
	}

	wh := make([]warehouseutils.Warehouse, 0)

	if sourceID != "" && destID == "" {
		// get all wh destinations for given source id
		connectionsMapLock.Lock()
		for _, srcMap := range connectionsMap {
			for srcID, w := range srcMap {
				if srcID == sourceID {
					wh = append(wh, w)
				}
			}
		}
		connectionsMapLock.Unlock()
	}
	if destID != "" {
		connectionsMapLock.Lock()
		for destinationId, srcMap := range connectionsMap {
			if destinationId == destID {
				for _, w := range srcMap {
					wh = append(wh, w)
				}
			}
		}
		connectionsMapLock.Unlock()
	}

	// return error if no such destinations found
	if len(wh) == 0 {
		err := fmt.Errorf("no warehouse destinations found for source id '%s'", sourceID)
		pkgLogger.Errorf("[WH]: %v", err)
		return err
	}

	// iterate over each wh destination and trigger upload
	for _, warehouse := range wh {
		triggerUpload(warehouse)
	}
	return nil
}

func databricksVersionHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(deltalake.GetDatabricksVersion()))
}

func isUploadTriggered(wh warehouseutils.Warehouse) bool {
	triggerUploadsMapLock.Lock()
	isTriggered := triggerUploadsMap[wh.Identifier]
	triggerUploadsMapLock.Unlock()
	return isTriggered
}

func triggerUpload(wh warehouseutils.Warehouse) {
	triggerUploadsMapLock.Lock()
	triggerUploadsMap[wh.Identifier] = true
	triggerUploadsMapLock.Unlock()
	pkgLogger.Infof("[WH]: Upload triggered for warehouse '%s'", wh.Identifier)
}

func clearTriggeredUpload(wh warehouseutils.Warehouse) {
	triggerUploadsMapLock.Lock()
	delete(triggerUploadsMap, wh.Identifier)
	triggerUploadsMapLock.Unlock()
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	dbService := ""
	pgNotifierService := ""
	if runningMode != DegradedMode {
		if !CheckPGHealth(notifier.GetDBHandle()) {
			http.Error(w, "Cannot connect to pgNotifierService", http.StatusInternalServerError)
			return
		}
		pgNotifierService = "UP"
	}

	if isMaster() {
		if !CheckPGHealth(dbHandle) {
			http.Error(w, "Cannot connect to dbService", http.StatusInternalServerError)
			return
		}
		dbService = "UP"
	}

	healthVal := fmt.Sprintf(
		`{"server":"UP","db":%q,"pgNotifier":%q,"acceptingEvents":"TRUE","warehouseMode":%q,"goroutines":"%d"}`,
		dbService, pgNotifierService, strings.ToUpper(warehouseMode), runtime.NumGoroutine(),
	)
	w.Write([]byte(healthVal))
}

func getConnectionString() string {
	if !CheckForWarehouseEnvVars() {
		return misc.GetConnectionString()
	}
	return fmt.Sprintf("host=%s port=%d user=%s "+
		"password=%s dbname=%s sslmode=%s application_name=%s",
		host, port, user, password, dbname, sslMode, appName)
}

func startWebHandler(ctx context.Context) error {
	mux := http.NewServeMux()

	// do not register same endpoint when running embedded in rudder backend
	if isStandAlone() {
		mux.HandleFunc("/health", healthHandler)
	}
	if runningMode != DegradedMode {
		if isMaster() {
			pkgLogger.Infof("WH: Warehouse master service waiting for BackendConfig before starting on %d", webPort)
			backendconfig.DefaultBackendConfig.WaitForConfig(ctx)

			mux.Handle("/v1/process", (&api.WarehouseAPI{
				Logger: pkgLogger,
				Stats:  stats.Default,
				Repo: &repo.StagingFiles{
					DB: dbHandle,
				},
				Multitenant: tenantManager,
			}).Handler())

			// triggers upload only when there are pending events and triggerUpload is sent for a sourceId
			mux.HandleFunc("/v1/warehouse/pending-events", pendingEventsHandler)
			// triggers uploads for a source
			mux.HandleFunc("/v1/warehouse/trigger-upload", triggerUploadHandler)
			mux.HandleFunc("/databricksVersion", databricksVersionHandler)
			mux.HandleFunc("/v1/setConfig", setConfigHandler)

			// Warehouse Async Job end-points
			mux.HandleFunc("/v1/warehouse/jobs", asyncWh.AddWarehouseJobHandler)           // FIXME: add degraded mode
			mux.HandleFunc("/v1/warehouse/jobs/status", asyncWh.StatusWarehouseJobHandler) // FIXME: add degraded mode

			pkgLogger.Infof("WH: Starting warehouse master service in %d", webPort)
		} else {
			pkgLogger.Infof("WH: Starting warehouse slave service in %d", webPort)
		}
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", webPort),
		Handler: bugsnag.Handler(mux),
	}

	return httputil.ListenAndServe(ctx, srv)
}

// CheckForWarehouseEnvVars Checks if all the required Env Variables for Warehouse are present
func CheckForWarehouseEnvVars() bool {
	return config.IsSet("WAREHOUSE_JOBS_DB_HOST") &&
		config.IsSet("WAREHOUSE_JOBS_DB_USER") &&
		config.IsSet("WAREHOUSE_JOBS_DB_DB_NAME") &&
		config.IsSet("WAREHOUSE_JOBS_DB_PASSWORD")
}

// This checks if gateway is running or not
func isStandAlone() bool {
	return warehouseMode != EmbeddedMode && warehouseMode != EmbeddedMasterMode
}

func isMaster() bool {
	return warehouseMode == config.MasterMode ||
		warehouseMode == config.MasterSlaveMode ||
		warehouseMode == config.EmbeddedMode ||
		warehouseMode == config.EmbeddedMasterMode
}

func isSlave() bool {
	return warehouseMode == config.SlaveMode || warehouseMode == config.MasterSlaveMode || warehouseMode == config.EmbeddedMode
}

func isStandAloneSlave() bool {
	return warehouseMode == config.SlaveMode
}

func setupDB(ctx context.Context, connInfo string) error {
	if isStandAloneSlave() {
		return nil
	}

	var err error
	dbHandle, err = sql.Open("postgres", connInfo)
	if err != nil {
		return err
	}

	isDBCompatible, err := validators.IsPostgresCompatible(ctx, dbHandle)
	if err != nil {
		return err
	}

	if !isDBCompatible {
		err := errors.New("rudder Warehouse Service needs postgres version >= 10. Exiting")
		pkgLogger.Error(err)
		return err
	}

	if err = dbHandle.PingContext(ctx); err != nil {
		return fmt.Errorf("could not ping WH db: %w", err)
	}

	return setupTables(dbHandle)
}

// Setup prepares the database connection for warehouse service, verifies database compatibility and creates the required tables
func Setup(ctx context.Context) error {
	if !isStandAlone() && !db.IsNormalMode() {
		return nil
	}
	psqlInfo := getConnectionString()
	if err := setupDB(ctx, psqlInfo); err != nil {
		return fmt.Errorf("cannot setup warehouse db: %w", err)
	}
	return nil
}

// Start starts the warehouse service
func Start(ctx context.Context, app app.App) error {
	application = app

	if dbHandle == nil && !isStandAloneSlave() {
		return errors.New("warehouse service cannot start, database connection is not setup")
	}
	// do not start warehouse service if rudder core is not in normal mode and warehouse is running in same process as rudder core
	if !isStandAlone() && !db.IsNormalMode() {
		pkgLogger.Infof("Skipping start of warehouse service...")
		return nil
	}

	pkgLogger.Infof("WH: Starting Warehouse service...")
	psqlInfo := getConnectionString()

	defer func() {
		if r := recover(); r != nil {
			pkgLogger.Fatal(r)
			panic(r)
		}
	}()

	runningMode := config.GetString("Warehouse.runningMode", "")
	if runningMode == DegradedMode {
		pkgLogger.Infof("WH: Running warehouse service in degraded mode...")
		if isMaster() {
			rruntime.GoForWarehouse(func() {
				minimalConfigSubscriber()
			})
			err := InitWarehouseAPI(dbHandle, pkgLogger.Child("upload_api"))
			if err != nil {
				pkgLogger.Errorf("WH: Failed to start warehouse api: %v", err)
				return err
			}
		}
		return startWebHandler(ctx)
	}
	var err error
	workspaceIdentifier := fmt.Sprintf(`%s::%s`, config.GetKubeNamespace(), misc.GetMD5Hash(config.GetWorkspaceToken()))
	notifier, err = pgnotifier.New(workspaceIdentifier, psqlInfo)
	if err != nil {
		return fmt.Errorf("cannot setup pgnotifier: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// Setting up reporting client
	// only if standalone or embedded connecting to diff DB for warehouse
	if (isStandAlone() && isMaster()) || (misc.GetConnectionString() != psqlInfo) {
		reporting := application.Features().Reporting.Setup(backendconfig.DefaultBackendConfig)

		g.Go(misc.WithBugsnagForWarehouse(func() error {
			reporting.AddClient(ctx, types.Config{ConnInfo: psqlInfo, ClientName: types.WarehouseReportingClient})
			return nil
		}))
	}

	if isStandAlone() && isMaster() {
		destinationdebugger.Setup(backendconfig.DefaultBackendConfig)

		// Report warehouse features
		g.Go(func() error {
			backendconfig.DefaultBackendConfig.WaitForConfig(ctx)

			c := controlplane.NewClient(
				backendconfig.GetConfigBackendURL(),
				backendconfig.DefaultBackendConfig.Identity(),
			)

			err := c.SendFeatures(ctx, info.WarehouseComponent.Name, info.WarehouseComponent.Features)
			if err != nil {
				pkgLogger.Errorf("error sending warehouse features: %v", err)
			}

			// We don't want to exit if we fail to send features
			return nil
		})
	}

	if isSlave() {
		pkgLogger.Infof("WH: Starting warehouse slave...")
		g.Go(misc.WithBugsnagForWarehouse(func() error {
			return setupSlave(ctx)
		}))
	}

	if isMaster() {
		pkgLogger.Infof("[WH]: Starting warehouse master...")

		backendconfig.DefaultBackendConfig.WaitForConfig(ctx)

		region := config.GetString("region", "")

		controlPlaneClient = controlplane.NewClient(
			backendconfig.GetConfigBackendURL(),
			backendconfig.DefaultBackendConfig.Identity(),
			controlplane.WithRegion(region),
		)

		tenantManager = &multitenant.Manager{
			BackendConfig: backendconfig.DefaultBackendConfig,
		}
		g.Go(func() error {
			tenantManager.Run(ctx)
			return nil
		})

		g.Go(misc.WithBugsnagForWarehouse(func() error {
			return notifier.ClearJobs(ctx)
		}))

		g.Go(misc.WithBugsnagForWarehouse(func() error {
			monitorDestRouters(ctx)
			return nil
		}))

		archiver := &archive.Archiver{
			DB:          dbHandle,
			Stats:       stats.Default,
			Logger:      pkgLogger.Child("archiver"),
			FileManager: filemanager.DefaultFileManagerFactory,
			Multitenant: tenantManager,
		}
		g.Go(misc.WithBugsnagForWarehouse(func() error {
			archive.CronArchiver(ctx, archiver)
			return nil
		}))

		err := InitWarehouseAPI(dbHandle, pkgLogger.Child("upload_api"))
		if err != nil {
			pkgLogger.Errorf("WH: Failed to start warehouse api: %v", err)
			return err
		}
		asyncWh = jobs.InitWarehouseJobsAPI(ctx, dbHandle, &notifier)
		jobs.WithConfig(asyncWh, config.Default)

		g.Go(misc.WithBugsnagForWarehouse(func() error {
			return asyncWh.InitAsyncJobRunner()
		}))
	}

	g.Go(func() error {
		return startWebHandler(ctx)
	})

	return g.Wait()
}
