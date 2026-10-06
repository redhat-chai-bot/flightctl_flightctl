package kvstore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/kvstore"
	flightlog "github.com/flightctl/flightctl/pkg/log"
	"github.com/flightctl/flightctl/test/integration/integrationstack"
	testutil "github.com/flightctl/flightctl/test/util"
	"github.com/flightctl/flightctl/test/util/testdb"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
)

var (
	suiteCtx      context.Context
	redisHost     string
	redisPort     uint
	redisPassword domain.SecureString
	redisCleanup  func()
)

func TestStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KVstore Suite")
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
	suiteCtx = testutil.InitSuiteTracerForGinkgo("KVstore Suite")
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

var _ = Describe("FleetSelector", func() {
	var (
		ctx     context.Context
		orgId   uuid.UUID
		kvStore kvstore.KVStore
		log     *logrus.Logger
	)

	BeforeEach(func() {
		ctx = testutil.StartSpecTracerForGinkgo(suiteCtx)
		orgId, _ = uuid.NewUUID()
		log = flightlog.InitLogs()
		var err error
		kvStore, err = kvstore.NewKVStore(ctx, log, redisHost, redisPort, redisPassword)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		kvStore.Close()
	})

	When("fetching a git revision", func() {
		It("returns what is stored if the key exists", func() {
			key := kvstore.GitRevisionKey{
				OrgID:           orgId,
				Fleet:           "myfleet",
				TemplateVersion: "mytv",
				Repository:      "myrepo",
				TargetRevision:  "main",
			}

			updated, err := kvStore.SetNX(ctx, key.ComposeKey(), []byte("abc123"))
			Expect(err).ToNot(HaveOccurred())
			Expect(updated).To(BeTrue())

			updated, err = kvStore.SetNX(ctx, key.ComposeKey(), []byte("def456"))
			Expect(err).ToNot(HaveOccurred())
			Expect(updated).To(BeFalse())

			hash, err := kvStore.Get(ctx, key.ComposeKey())
			Expect(err).ToNot(HaveOccurred())
			Expect(hash).To(Equal([]byte("abc123")))
		})

		It("returns an empty string if the key doesn't exist", func() {
			key := kvstore.GitRevisionKey{
				OrgID:           orgId,
				Fleet:           "myfleet",
				TemplateVersion: "mytv",
				Repository:      "myrepo",
				TargetRevision:  "main",
			}

			hash, err := kvStore.Get(ctx, key.ComposeKey())
			Expect(err).ToNot(HaveOccurred())
			Expect(hash).To(HaveLen(0))
		})
	})

	When("setting a repo URL", func() {
		It("stores what is passed if the key doesn't exist", func() {
			key := kvstore.RepositoryUrlKey{
				OrgID:           orgId,
				Fleet:           "myfleet",
				TemplateVersion: "mytv",
				Repository:      "myrepo",
			}

			url, err := kvStore.GetOrSetNX(ctx, key.ComposeKey(), []byte("https://myurl"))
			Expect(err).ToNot(HaveOccurred())
			Expect(url).To(Equal([]byte("https://myurl")))
		})

		It("returns what is stored if the key exists", func() {
			key := kvstore.RepositoryUrlKey{
				OrgID:           orgId,
				Fleet:           "myfleet",
				TemplateVersion: "mytv",
				Repository:      "myrepo",
			}

			url, err := kvStore.GetOrSetNX(ctx, key.ComposeKey(), []byte("https://myurl"))
			Expect(err).ToNot(HaveOccurred())
			Expect(url).To(Equal([]byte("https://myurl")))

			url, err = kvStore.GetOrSetNX(ctx, key.ComposeKey(), []byte("https://otherurl"))
			Expect(err).ToNot(HaveOccurred())
			Expect(url).To(Equal([]byte("https://myurl")))
		})
	})

	When("deleting a TemplateVersion", func() {
		It("deletes all its related keys", func() {
			key := kvstore.RepositoryUrlKey{
				OrgID:           orgId,
				Fleet:           "myfleet",
				TemplateVersion: "mytv",
				Repository:      "myrepo",
			}

			updated, err := kvStore.SetNX(ctx, key.ComposeKey(), []byte("https://myurl"))
			Expect(err).ToNot(HaveOccurred())
			Expect(updated).To(BeTrue())
			key.TemplateVersion = "othertv"
			updated, err = kvStore.SetNX(ctx, key.ComposeKey(), []byte("https://otherurl"))
			Expect(err).ToNot(HaveOccurred())
			Expect(updated).To(BeTrue())

			key2 := kvstore.GitRevisionKey{
				OrgID:           orgId,
				Fleet:           "myfleet",
				TemplateVersion: "mytv",
				Repository:      "myrepo",
				TargetRevision:  "main",
			}

			updated, err = kvStore.SetNX(ctx, key2.ComposeKey(), []byte("abc123"))
			Expect(err).ToNot(HaveOccurred())
			Expect(updated).To(BeTrue())
			key2.TemplateVersion = "othertv"
			updated, err = kvStore.SetNX(ctx, key2.ComposeKey(), []byte("def456"))
			Expect(err).ToNot(HaveOccurred())
			Expect(updated).To(BeTrue())

			tvkey := kvstore.TemplateVersionKey{OrgID: orgId, Fleet: "myfleet", TemplateVersion: "mytv"}
			err = kvStore.DeleteKeysForTemplateVersion(ctx, tvkey.ComposeKey())
			Expect(err).ToNot(HaveOccurred())

			ret, err := kvStore.Get(ctx, key.ComposeKey())
			Expect(err).ToNot(HaveOccurred())
			Expect(ret).To(Equal([]byte("https://otherurl")))
			ret, err = kvStore.Get(ctx, key2.ComposeKey())
			Expect(err).ToNot(HaveOccurred())
			Expect(ret).To(Equal([]byte("def456")))

			key.TemplateVersion = "mytv"
			key2.TemplateVersion = "mytv"
			ret, err = kvStore.Get(ctx, key.ComposeKey())
			Expect(err).ToNot(HaveOccurred())
			Expect(ret).To(BeEmpty())
			ret, err = kvStore.Get(ctx, key2.ComposeKey())
			Expect(err).ToNot(HaveOccurred())
			Expect(ret).To(BeEmpty())
		})
	})
})
