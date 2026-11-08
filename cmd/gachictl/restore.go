package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/tym83/gachinetes/pkg/replica"
	"github.com/tym83/gachinetes/pkg/tape"
)

type restoreFlags struct {
	from, endpoint, region, identity string
	at, dataDir, out, storeBin       string
	name, initialCluster, peers      string
	leaseTTL                         time.Duration
}

// tapeRestoreCmd brings the store back from the tape replicated to S3
// (gachi-tape --replicate), after the whole cluster is lost.
func tapeRestoreCmd() *cobra.Command {
	var f restoreFlags
	c := &cobra.Command{
		Use:   "restore --from s3://BUCKET/PREFIX [--at TIME|REVISION] --data-dir DIR",
		Short: "Bring a store back from the tape replicated to S3",
		Long: `Reads the base and the journal gachi-tape shipped to S3 and makes a store
member of them, up to a revision or a moment, with the tape (rewind,
history, graveyard, undelete) from the oldest base the bucket keeps.

The keys come from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. --data-dir
runs kuberoot-store tape restore; --out only writes the tape file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (f.dataDir == "") == (f.out == "") {
				return fmt.Errorf("--data-dir or --out, one of them")
			}
			var target replica.Target
			if f.at != "" {
				v, err := parseWhen(f.at)
				if err != nil {
					return err
				}
				p, _ := tape.ParsePoint(v)
				target = replica.Target{Revision: p.Rev, At: p.At}
			}
			b, err := replica.ParseBucket(f.from, f.endpoint, f.region)
			if err != nil {
				return err
			}
			ids, err := replica.Identities(f.identity)
			if err != nil {
				return err
			}
			file := f.out
			if file == "" {
				dir, err := os.MkdirTemp("", "gachi-tape-restore-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(dir)
				file = filepath.Join(dir, "tape.jsonl")
			}
			out, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			res, err := replica.WriteTapeFile(cmd.Context(), b, ids, target, out)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "♂ the tape from %s: revisions %d … %d (%s … %s), %d versions after the base\n",
				b, res.Base, res.Revision, when(res.BaseTime), when(res.Time), res.Versions)
			if f.dataDir == "" {
				fmt.Fprintf(os.Stderr, "tape file %s; kuberoot-store tape restore %s --data-dir DIR --revision %d\n", file, file, res.Revision)
				return nil
			}
			args := []string{"tape", "restore", file, "--data-dir", f.dataDir, "--revision", strconv.FormatInt(res.Revision, 10),
				"--lease-ttl", f.leaseTTL.String(), "--name", f.name}
			if f.initialCluster != "" {
				args = append(args, "--initial-cluster", f.initialCluster)
			}
			if f.peers != "" {
				args = append(args, "--initial-advertise-peer-urls", f.peers)
			}
			run := exec.CommandContext(cmd.Context(), f.storeBin, args...)
			run.Stdout, run.Stderr = os.Stdout, os.Stderr
			if err := run.Run(); err != nil {
				return fmt.Errorf("%s tape restore: %w", f.storeBin, err)
			}
			fmt.Fprintf(os.Stderr, "Rest in peace? Not today. Start kuberoot-store --data-dir %s --keep-history, and the API server on it.\n", f.dataDir)
			return nil
		},
	}
	c.Flags().StringVar(&f.from, "from", "", "the bucket and prefix gachi-tape replicates to, s3://BUCKET/PREFIX")
	c.Flags().StringVar(&f.endpoint, "s3-endpoint", "", "the S3 endpoint, https://… (default: AWS's for the region)")
	c.Flags().StringVar(&f.region, "s3-region", "us-east-1", "the S3 region")
	c.Flags().StringVar(&f.identity, "age-identity", "", "a file of age identities, to open a tape replicated encrypted")
	c.Flags().StringVar(&f.at, "at", "", "restore up to a revision, an RFC 3339 time or a duration ago (default: all the bucket holds)")
	c.Flags().StringVar(&f.dataDir, "data-dir", "", "make a store member here, with kuberoot-store tape restore")
	c.Flags().StringVar(&f.out, "out", "", "only write the tape file here")
	c.Flags().StringVar(&f.storeBin, "kuberoot-store", "kuberoot-store", "the kuberoot-store binary")
	c.Flags().StringVar(&f.name, "name", "default", "the member's name")
	c.Flags().StringVar(&f.initialCluster, "initial-cluster", "", "the new store's members, name=peer-URL,… (default: this member alone)")
	c.Flags().StringVar(&f.peers, "initial-advertise-peer-urls", "", "this member's peer URLs, when --initial-cluster is not given")
	c.Flags().DurationVar(&f.leaseTTL, "lease-ttl", time.Hour, "the time to live of the leases restored keys are attached to")
	_ = c.MarkFlagRequired("from")
	return c
}
