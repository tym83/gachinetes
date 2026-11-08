package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/tym83/gachinetes/pkg/replica"
	"github.com/tym83/gachinetes/pkg/tape"
)

// replication is where and how the tape is shipped.
type replication struct {
	to, endpoint, region, recipientsFile string
	recipients                           stringList
	accessKey, secretKey                 string
	allowUnencrypted                     bool
	interval, baseEvery, retention       time.Duration
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func (r *replication) register() {
	flag.StringVar(&r.to, "replicate", "", "ship the tape to S3 as it is written, s3://BUCKET/PREFIX; keys from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	flag.StringVar(&r.endpoint, "s3-endpoint", "", "the S3 endpoint, https://… (default: AWS's for the region)")
	flag.StringVar(&r.region, "s3-region", "us-east-1", "the S3 region")
	flag.StringVar(&r.recipientsFile, "age-recipients-file", "", "age public keys, one per line, the replica is encrypted to")
	flag.Var(&r.recipients, "age-recipient", "an age public key the replica is encrypted to (repeatable)")
	flag.BoolVar(&r.allowUnencrypted, "s3-allow-unencrypted", false, "replicate without age encryption")
	flag.DurationVar(&r.interval, "replicate-interval", 30*time.Second, "how often the journal is shipped")
	flag.DurationVar(&r.baseEvery, "replicate-base-every", 24*time.Hour, "how often a new base (every key) is shipped")
	flag.DurationVar(&r.retention, "replicate-retention", 0, "how long the bucket keeps the tape, such as 720h; 0 keeps everything")
}

// fromSecret is the replication a Secret asks for, in the keys kuberoot's
// state backups take: bucket (s3://BUCKET/PREFIX), s3Endpoint, s3Region,
// s3AccessKey, s3SecretKey, encryptionRecipient (age public keys, one per
// line), allowUnencrypted, and interval, baseEvery, retention.
func fromSecret(data map[string][]byte) (replication, error) {
	get := func(k string) string { return strings.TrimSpace(string(data[k])) }
	r := replication{to: get("bucket"), endpoint: get("s3Endpoint"), region: get("s3Region"),
		accessKey: get("s3AccessKey"), secretKey: get("s3SecretKey"), allowUnencrypted: get("allowUnencrypted") == "true",
		interval: 30 * time.Second, baseEvery: 24 * time.Hour}
	if r.region == "" {
		r.region = "us-east-1"
	}
	if v := get("encryptionRecipient"); v != "" {
		r.recipients = strings.Split(v, "\n")
	}
	for k, d := range map[string]*time.Duration{"interval": &r.interval, "baseEvery": &r.baseEvery, "retention": &r.retention} {
		if v := get(k); v != "" {
			p, err := time.ParseDuration(v)
			if err != nil {
				return r, fmt.Errorf("%s: %w", k, err)
			}
			*d = p
		}
	}
	if r.to == "" {
		return r, fmt.Errorf("bucket is required")
	}
	return r, nil
}

// shipper makes the replication's shipper.
func (r replication) shipper(storeURL string) (*replica.Shipper, error) {
	if r.accessKey != "" {
		os.Setenv("AWS_ACCESS_KEY_ID", r.accessKey)
		os.Setenv("AWS_SECRET_ACCESS_KEY", r.secretKey)
	}
	b, err := replica.ParseBucket(r.to, r.endpoint, r.region)
	if err != nil {
		return nil, err
	}
	rs, err := replica.Recipients(r.recipientsFile, r.recipients)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 && !r.allowUnencrypted {
		return nil, fmt.Errorf("replicating needs age recipients: the tape holds the whole cluster (or allow it unencrypted)")
	}
	return &replica.Shipper{
		// The values as the store holds them: never decrypted.
		Tape:   &tape.Store{URL: storeURL, Client: &http.Client{Timeout: 5 * time.Minute}},
		Bucket: b, Recipients: rs, BaseEvery: r.baseEvery, Retention: r.retention,
	}, nil
}

// replicate ships the tape while this member's gachi-tape holds the
// replicator's lease: one shipper for the cluster, whichever control plane.
// With --replicate it ships where the flags say; otherwise where the
// Secret gachi-tape-replica in the namespace says, when there is one,
// following its changes.
func replicate(ctx context.Context, flags replication, cs kubernetes.Interface, storeURL, namespace string) {
	if flags.to != "" {
		ship, err := flags.shipper(storeURL)
		if err != nil {
			log.Fatalf("--replicate: %v", err)
		}
		lead(ctx, cs, namespace, ship, flags.interval)
		return
	}
	stop := context.CancelFunc(func() {})
	defer func() { stop() }()
	running := false
	var seen [32]byte
	for {
		s, err := cs.CoreV1().Secrets(namespace).Get(ctx, "gachi-tape-replica", metav1.GetOptions{})
		var sum [32]byte
		if err == nil {
			sum = digest(s.Data)
		}
		switch {
		case err != nil && !apierrors.IsNotFound(err):
			log.Printf("tape replica: %v", err)
		case sum != seen:
			seen = sum
			if running {
				stop()
				running = false
				log.Printf("tape replica: the Secret changed")
			}
			if err != nil {
				break
			}
			r, err := fromSecret(s.Data)
			var ship *replica.Shipper
			if err == nil {
				ship, err = r.shipper(storeURL)
			}
			if err != nil {
				log.Printf("tape replica: Secret %s/gachi-tape-replica: %v", namespace, err)
				break
			}
			c, cancel := context.WithCancel(ctx)
			stop, running = cancel, true
			go lead(c, cs, namespace, ship, r.interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func digest(data map[string][]byte) [32]byte {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%x;", k, data[k])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// lead ships while holding the lease gachi-tape-replicator.
func lead(ctx context.Context, cs kubernetes.Interface, namespace string, ship *replica.Shipper, interval time.Duration) {
	host, _ := os.Hostname()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: "gachi-tape-replicator", Namespace: namespace},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: fmt.Sprintf("%s_%d", host, time.Now().UnixNano())},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second,
		ReleaseOnCancel: true, Name: "gachi-tape-replicator",
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Printf("♂ tape replica: shipping to %s (encrypted: %v)", ship.Bucket, len(ship.Recipients) > 0)
				ship.Run(ctx, interval)
			},
			OnStoppedLeading: func() { log.Printf("tape replica: not shipping (not the replicator)") },
		},
	})
}
