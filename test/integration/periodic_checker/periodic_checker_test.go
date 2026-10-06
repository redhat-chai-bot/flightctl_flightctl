package periodic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flightctl/flightctl/internal/config"
	"github.com/flightctl/flightctl/internal/consts"
	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/kvstore"
	periodic "github.com/flightctl/flightctl/internal/periodic_checker"
	organizationservice "github.com/flightctl/flightctl/internal/service/organization"
	"github.com/flightctl/flightctl/internal/store"
	"github.com/flightctl/flightctl/internal/store/model"
	organizationstore "github.com/flightctl/flightctl/internal/store/organization"
	flightlog "github.com/flightctl/flightctl/pkg/log"
	"github.com/flightctl/flightctl/pkg/poll"
	"github.com/flightctl/flightctl/pkg/queues"
	"github.com/flightctl/flightctl/test/integration/integrationstack"
	testutil "github.com/flightctl/flightctl/test/util"
	"github.com/flightctl/flightctl/test/util/testdb"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
)

var (
	suiteCtx      context.Context
	redisHost     string
	redisPort     uint
	redisPassword domain.SecureString
	redisCleanup  func()
)

// Mock task executor for testing
type mockPeriodicTaskExecutor struct {
	mu               sync.Mutex
	executeCallCount int
	orgIDs           []uuid.UUID // Store org IDs received during execution
	panic            bool
}

func (m *mockPeriodicTaskExecutor) Execute(ctx context.Context, log logrus.FieldLogger, orgID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executeCallCount++
	m.orgIDs = append(m.orgIDs, orgID)

	// Panic last so we record the call count and args first
	if m.panic {
		panic("test panic")
	}
}

func (m *mockPeriodicTaskExecutor) GetExecuteCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.executeCallCount
}

func (m *mockPeriodicTaskExecutor) GetOrgIDs() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]uuid.UUID, len(m.orgIDs))
	copy(result, m.orgIDs)
	return result
}

// Test periodic task metadata for publisher
var testPeriodicTasks = map[periodic.PeriodicTaskType]periodic.PeriodicTaskMetadata{
	periodic.PeriodicTaskTypeRepositoryTester: {
		Interval: 5 * time.Second,
	},
	periodic.PeriodicTaskTypeResourceSync: {
		Interval: 1 * time.Second,
	},
}

func TestStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Periodic Suite")
}

var _ = SynchronizedBeforeSuite(func(ctx context.Context) []byte {
	// Proc 1 only: start shared infrastructure and single Redis container.
	Expect(integrationstack.EnsureRunning(ctx)).To(Succeed())
	host, port, password, cleanup, err := testdb.CreateTestRedis(ctx, flightlog.InitLogs())
	Expect(err).ToNot(HaveOccurred())
	redisCleanup = cleanup
	info, err := json.Marshal(map[string]interface{}{
		"host": host, "port": port, "password": string(password),
	})
	Expect(err).ToNot(HaveOccurred())
	return info
}, func(ctx context.Context, data []byte) {
	// All procs: deserialize shared Redis connection and set up per-process state.
	suiteCtx = testutil.InitSuiteTracerForGinkgo("Periodic Suite")
	Expect(integrationstack.EnsureRunning(suiteCtx)).To(Succeed())
	var info map[string]interface{}
	Expect(json.Unmarshal(data, &info)).To(Succeed())
	redisHost = info["host"].(string)
	redisPort = uint(info["port"].(float64))
	redisPassword = domain.SecureString(info["password"].(string))
})

var _ = SynchronizedAfterSuite(func() {
	// All procs: no per-process cleanup needed (Redis is shared).
}, func() {
	// Proc 1 only: tear down the shared Redis container.
	if redisCleanup != nil {
		redisCleanup()
	}
})

var _ = Describe("Periodic", func() {
	var (
		ctx                      context.Context
		kvStore                  kvstore.KVStore
		log                      *logrus.Logger
		queuesProvider           queues.Provider
		organizationSvc          organizationservice.Service
		organizationStore        organizationstore.Store
		cfg                      *config.Config
		dbName                   string
		db                       *gorm.DB
		orgId                    uuid.UUID
		publisherConfig          periodic.PeriodicTaskPublisherConfig
		consumerConfig           periodic.PeriodicTaskConsumerConfig
		repositoryTesterExecutor *mockPeriodicTaskExecutor
		resourceSyncExecutor     *mockPeriodicTaskExecutor
		cancel                   context.CancelFunc
		channelManager           *periodic.ChannelManager
	)

	BeforeEach(func() {
		baseCtx := testutil.StartSpecTracerForGinkgo(suiteCtx)
		baseCtx = context.WithValue(baseCtx, consts.EventSourceComponentCtxKey, "flightctl-periodic")
		baseCtx = context.WithValue(baseCtx, consts.EventActorCtxKey, "service:flightctl-periodic")
		ctx, cancel = context.WithCancel(baseCtx)

		log = flightlog.InitLogs()

		// Setup database and store
		var err error
		cfg, dbName, db, err = testdb.CreateTestDB(ctx, log, "", store.InitDB)
		Expect(err).NotTo(HaveOccurred())
		organizationStore = organizationstore.NewOrganizationStore(db)

		// Grab default org id from the database
		orgs, err := organizationStore.List(ctx, store.ListParams{})
		Expect(err).ToNot(HaveOccurred())
		if len(orgs) > 0 {
			orgId = orgs[0].ID
		} else {
			Fail("No orgs found in database")
		}

		processID := fmt.Sprintf("periodic-test-%s", uuid.New().String())
		queuesProvider, err = queues.NewRedisProvider(ctx, log, processID, redisHost, redisPort, redisPassword, queues.DefaultRetryConfig())
		Expect(err).ToNot(HaveOccurred())

		// Setup kvStore for test
		kvStore, err = kvstore.NewKVStore(ctx, log, redisHost, redisPort, redisPassword)
		Expect(err).ToNot(HaveOccurred())

		organizationSvc = organizationservice.WrapWithTracing(organizationservice.NewServiceHandler(organizationStore))

		channelManager, err = periodic.NewChannelManager(periodic.ChannelManagerConfig{
			Log: log,
		})
		Expect(err).ToNot(HaveOccurred())

		publisherConfig = periodic.PeriodicTaskPublisherConfig{
			Log:            log,
			OrgService:     organizationSvc,
			TasksMetadata:  testPeriodicTasks,
			ChannelManager: channelManager,
			TaskBackoff: &poll.Config{
				BaseDelay: 1 * time.Minute, // Make backoff very long to make it clear if it is triggered
				Factor:    2,
				MaxDelay:  2 * time.Minute,
			},
		}

		repositoryTesterExecutor = &mockPeriodicTaskExecutor{}
		resourceSyncExecutor = &mockPeriodicTaskExecutor{}
		executors := map[periodic.PeriodicTaskType]periodic.PeriodicTaskExecutor{
			periodic.PeriodicTaskTypeRepositoryTester: repositoryTesterExecutor,
			periodic.PeriodicTaskTypeResourceSync:     resourceSyncExecutor,
		}
		consumerConfig = periodic.PeriodicTaskConsumerConfig{
			ChannelManager: channelManager,
			Log:            log,
			Executors:      executors,
			ConsumerCount:  3,
		}
	})

	AfterEach(func() {
		// Stop queues provider
		if queuesProvider != nil {
			queuesProvider.Stop()
		}

		// Close kvStore (no need to DeleteAllKeys - ephemeral container handles cleanup)
		if kvStore != nil {
			kvStore.Close()
		}

		// Clean up database
		Expect(testdb.DeleteTestDB(ctx, log, cfg, db, dbName)).To(Succeed())

		// Clean up channel manager
		if channelManager != nil {
			channelManager.Close()
		}

		if cancel != nil {
			cancel()
		}
	})

	When("running periodic tasks", func() {
		It("runs tasks on configured intervals", func() {
			// Start consumer and publisher together
			eg, egCtx := errgroup.WithContext(ctx)

			periodicTaskConsumer, err := periodic.NewPeriodicTaskConsumer(consumerConfig)
			Expect(err).ToNot(HaveOccurred())
			eg.Go(func() error {
				periodicTaskConsumer.Run(egCtx)
				return nil
			})

			periodicTaskPublisher, err := periodic.NewPeriodicTaskPublisher(publisherConfig)
			Expect(err).ToNot(HaveOccurred())
			eg.Go(func() error {
				periodicTaskPublisher.Run(egCtx)
				return nil
			})

			// Give some time for tasks to be processed and published
			Eventually(func() bool {
				return repositoryTesterExecutor.GetExecuteCallCount() >= 1 && resourceSyncExecutor.GetExecuteCallCount() >= 4
			}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
		})

		It("works for multiple organizations", func() {
			externalID2 := "external-id-2"
			// Create a second organization
			orgId2 := uuid.New()
			org := &model.Organization{
				ID:          orgId2,
				DisplayName: "test-org-2",
				ExternalID:  externalID2,
			}
			_, err := organizationStore.Create(ctx, org)
			Expect(err).ToNot(HaveOccurred())

			// Start consumer and publisher together
			eg, egCtx := errgroup.WithContext(ctx)

			periodicTaskConsumer, err := periodic.NewPeriodicTaskConsumer(consumerConfig)
			Expect(err).ToNot(HaveOccurred())
			eg.Go(func() error {
				periodicTaskConsumer.Run(egCtx)
				return nil
			})

			periodicTaskPublisher, err := periodic.NewPeriodicTaskPublisher(publisherConfig)
			Expect(err).ToNot(HaveOccurred())
			eg.Go(func() error {
				periodicTaskPublisher.Run(egCtx)
				return nil
			})

			// Repository tester should be called once for each org since it has a minute interval
			// Resource sync should be called numerous times for each org as it is re-enqueued with a 1 second interval
			Eventually(func() bool {
				repoTesterOrgIds := repositoryTesterExecutor.GetOrgIDs()
				resourceSyncOrgIds := resourceSyncExecutor.GetOrgIDs()

				if !slices.Contains(repoTesterOrgIds, orgId2) || !slices.Contains(resourceSyncOrgIds, orgId2) ||
					!slices.Contains(repoTesterOrgIds, orgId) || !slices.Contains(resourceSyncOrgIds, orgId) {
					return false
				}

				// Short circuit after ~5 calls per org to resource sync which should be called roughly every second
				if repositoryTesterExecutor.GetExecuteCallCount() < 2 && resourceSyncExecutor.GetExecuteCallCount() < 10 {
					return false
				}

				return true
			}, 10*time.Second, 1*time.Second).Should(BeTrue())
		})
	})
})
