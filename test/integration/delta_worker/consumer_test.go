package delta_worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/flightctl/flightctl/internal/consts"
	deltaconfig "github.com/flightctl/flightctl/internal/delta_worker/config"
	deltatasks "github.com/flightctl/flightctl/internal/delta_worker/tasks"
	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/worker_client"
	flightlog "github.com/flightctl/flightctl/pkg/log"
	"github.com/flightctl/flightctl/pkg/queues"
	"github.com/flightctl/flightctl/test/integration/integrationstack"
	testutil "github.com/flightctl/flightctl/test/util"
	"github.com/flightctl/flightctl/test/util/testdb"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

var (
	suiteCtx      context.Context
	redisHost     string
	redisPort     uint
	redisPassword domain.SecureString
	redisClient   *redis.Client
	redisCleanup  func()
)

func TestDeltaWorker(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Delta Worker Suite")
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
	suiteCtx = testutil.InitSuiteTracerForGinkgo("Delta Worker Suite")
	Expect(integrationstack.EnsureRunning(suiteCtx)).To(Succeed())
	var info map[string]interface{}
	Expect(json.Unmarshal(data, &info)).To(Succeed())
	redisHost = info["host"].(string)
	redisPort = uint(info["port"].(float64))
	redisPassword = domain.SecureString(info["password"].(string))

	redisClient = redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%d", redisHost, redisPort),
		Password: string(redisPassword),
		DB:       0,
	})
})

var _ = SynchronizedAfterSuite(func() {
	// All procs: close per-process Redis client.
	if redisClient != nil {
		Expect(redisClient.Close()).To(Succeed())
	}
}, func() {
	// Proc 1 only: tear down the shared Redis container.
	if redisCleanup != nil {
		redisCleanup()
	}
})

var _ = Describe("Delta worker consumers", func() {
	var (
		ctx      context.Context
		cancel   context.CancelFunc
		log      *logrus.Logger
		provider queues.Provider
	)

	BeforeEach(func() {
		baseCtx := testutil.StartSpecTracerForGinkgo(suiteCtx)
		ctx, cancel = context.WithCancel(baseCtx)
		log = flightlog.InitLogs()

		processID := fmt.Sprintf("delta-worker-test-%s", uuid.New().String())
		var err error
		provider, err = queues.NewRedisProvider(ctx, log, processID, redisHost, redisPort, redisPassword, queues.DefaultRetryConfig())
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		if provider != nil {
			provider.Stop()
			provider.Wait()
		}
		if redisClient != nil {
			Expect(redisClient.Del(ctx, consts.DeltaGenerationTaskQueue, consts.TaskQueue).Err()).To(Succeed())
		}
		if cancel != nil {
			cancel()
		}
	})

	When("LaunchConsumers is running", func() {
		It("should consume and ack PrepareDeltas on DeltaGenerationTaskQueue", func() {
			cfg := &deltaconfig.DeltaGenerationConfig{MaxConcurrentDeltaGenerations: 1}
			Expect(deltatasks.LaunchConsumers(ctx, provider, cfg, nil, log, nil)).To(Succeed())

			payload := prepareDeltasPayload()
			producer, err := provider.NewQueueProducer(ctx, consts.DeltaGenerationTaskQueue)
			Expect(err).ToNot(HaveOccurred())
			defer producer.Close()
			Expect(producer.Enqueue(ctx, payload, time.Now().UnixMicro())).To(Succeed())

			Eventually(func() int64 {
				return streamLen(ctx, consts.DeltaGenerationTaskQueue)
			}, 10*time.Second, 100*time.Millisecond).Should(Equal(int64(0)))
		})

		It("should not consume the same payload from TaskQueue", func() {
			cfg := &deltaconfig.DeltaGenerationConfig{MaxConcurrentDeltaGenerations: 1}
			Expect(deltatasks.LaunchConsumers(ctx, provider, cfg, nil, log, nil)).To(Succeed())

			payload := prepareDeltasPayload()
			producer, err := provider.NewQueueProducer(ctx, consts.TaskQueue)
			Expect(err).ToNot(HaveOccurred())
			defer producer.Close()
			Expect(producer.Enqueue(ctx, payload, time.Now().UnixMicro())).To(Succeed())

			Consistently(func() int64 {
				return streamLen(ctx, consts.TaskQueue)
			}, 2*time.Second, 100*time.Millisecond).Should(Equal(int64(1)))
		})
	})
})

func prepareDeltasPayload() []byte {
	GinkgoHelper()
	body, err := json.Marshal(worker_client.EventWithOrgId{
		OrgId: uuid.New(),
		Event: domain.Event{Reason: domain.EventReasonPrepareDeltas},
	})
	Expect(err).ToNot(HaveOccurred())
	return body
}

func streamLen(ctx context.Context, queueName string) int64 {
	GinkgoHelper()
	n, err := redisClient.XLen(ctx, queueName).Result()
	Expect(err).ToNot(HaveOccurred())
	return n
}
