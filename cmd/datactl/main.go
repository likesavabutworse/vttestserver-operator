// Command datactl takes and restores snapshots of a vttestserver data dir. It has
// no Kubernetes dependency: the operator runs it in a Job and in an init container,
// and it also runs on a laptop against a docker volume.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot"
	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

// version is set at build time with -ldflags.
var version = "dev"

// newStore is a variable so that tests can use an in-memory store.
var newStore = func(ctx context.Context, o snapshot.S3Options) (snapshot.Store, error) {
	return snapshot.NewS3Store(ctx, o)
}

type commonFlags struct {
	s3             snapshot.S3Options
	terminationLog string
}

func (c *commonFlags) add(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&c.s3.Bucket, "bucket", "", "S3 bucket")
	f.StringVar(&c.s3.Prefix, "prefix", "", "key prefix inside the bucket")
	f.StringVar(&c.s3.Region, "region", "", "AWS region")
	f.StringVar(&c.s3.Endpoint, "endpoint", "", "endpoint URL of S3 compatible storage")
	f.BoolVar(&c.s3.ForcePathStyle, "force-path-style", false,
		"use path style addressing (needed by most S3 compatible storage)")
	f.StringVar(&c.terminationLog, "termination-log", "/dev/termination-log",
		"file for the result or error message that the operator reads")
	_ = cmd.MarkFlagRequired("bucket")
}

type fingerprintFlags struct {
	keyspaces []string
	file      string
}

func (f *fingerprintFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringArrayVar(&f.keyspaces, "keyspace", nil, "keyspace and shard count as name:shards, repeatable")
	fl.StringVar(&f.file, "fingerprint-file", "", `JSON file {"keyspaces":[{"name":"...","shards":1}]}`)
	cmd.MarkFlagsMutuallyExclusive("keyspace", "fingerprint-file")
	cmd.MarkFlagsOneRequired("keyspace", "fingerprint-file")
}

func (f *fingerprintFlags) load() (manifest.Fingerprint, error) {
	if f.file != "" {
		r, err := os.Open(f.file)
		if err != nil {
			return manifest.Fingerprint{}, err
		}
		defer func() { _ = r.Close() }()
		return manifest.ParseFingerprint(r)
	}
	return manifest.NewFingerprint(f.keyspaces)
}

// writeTermination records the outcome for the operator. A missing or read only
// termination log is normal outside Kubernetes, so a write error is ignored.
func writeTermination(path string, msg manifest.TerminationMessage) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, msg.Marshal(), 0o644)
}

// runWithTermination runs fn and, on error, records it to the termination log. fn is responsible for
// writing its own termination message on success, since only it knows what counts as a result.
func runWithTermination(terminationLog string, fn func() error) error {
	err := fn()
	if err != nil {
		writeTermination(terminationLog, manifest.TerminationMessage{Error: err.Error()})
	}
	return err
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "datactl",
		Short:         "Snapshot and restore the data dir of a vttestserver",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newSnapshotCmd(), newRestoreCmd(), newDeleteCmd(), newApplyMyCnfCmd())
	return root
}

func newSnapshotCmd() *cobra.Command {
	var (
		cf      commonFlags
		ff      fingerprintFlags
		dataDir string
		name    string
		tags    []string
		vitess  string
	)
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Archive the data dir and upload it. Stop mysqld first.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithTermination(cf.terminationLog, func() error {
				fp, err := ff.load()
				if err != nil {
					return err
				}
				st, err := newStore(cmd.Context(), cf.s3)
				if err != nil {
					return err
				}
				m, err := snapshot.Take(cmd.Context(), st, snapshot.TakeOptions{
					DataDir: dataDir, Name: name, Tags: tags, Fingerprint: fp, Vitess: vitess,
				})
				if err != nil {
					return err
				}
				writeTermination(cf.terminationLog,
					manifest.TerminationMessage{Name: m.Name, SHA256: m.SHA256, SizeBytes: m.SizeBytes})
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "snapshot %s: %d bytes, sha256 %s\n", m.Name, m.SizeBytes, m.SHA256)
				return nil
			})
		},
	}
	cf.add(cmd)
	ff.add(cmd)
	fl := cmd.Flags()
	fl.StringVar(&dataDir, "data-dir", "/vt/vtdataroot", "vttestserver data dir")
	fl.StringVar(&name, "name", "", "snapshot name")
	fl.StringArrayVar(&tags, "tag", nil, "tag to point at the snapshot, repeatable")
	fl.StringVar(&vitess, "vitess-version", "", "Vitess version to record in the manifest")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newRestoreCmd() *cobra.Command {
	var (
		cf      commonFlags
		ff      fingerprintFlags
		dataDir string
		name    string
		tag     string
		pinFile string
	)
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Download a snapshot into an empty or unfinished data dir",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithTermination(cf.terminationLog, func() error {
				fp, err := ff.load()
				if err != nil {
					return err
				}
				ref := snapshot.Ref{Name: name, Tag: tag}
				if pinFile != "" && tag != "" {
					// The pin is the snapshot that the first restore used. It wins over the tag,
					// so a new "latest" does not change a server that lost its volume.
					if b, err := os.ReadFile(pinFile); err == nil && strings.TrimSpace(string(b)) != "" {
						ref = snapshot.Ref{Name: strings.TrimSpace(string(b))}
					} else if err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
				st, err := newStore(cmd.Context(), cf.s3)
				if err != nil {
					return err
				}
				res, err := snapshot.Restore(cmd.Context(), st, dataDir, ref, fp)
				if err != nil {
					return err
				}
				writeTermination(cf.terminationLog, manifest.TerminationMessage{Name: res.Name, SHA256: res.SHA256})
				if res.Skipped {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "already restored from %s, nothing to do\n", res.Name)
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restored %s (sha256 %s)\n", res.Name, res.SHA256)
				}
				return nil
			})
		},
	}
	cf.add(cmd)
	ff.add(cmd)
	fl := cmd.Flags()
	fl.StringVar(&dataDir, "data-dir", "/vt/vtdataroot", "vttestserver data dir")
	fl.StringVar(&name, "name", "", "snapshot name")
	fl.StringVar(&tag, "tag", "", "tag to resolve, for example latest")
	fl.StringVar(&pinFile, "pin-file", "", "file with a snapshot name that takes the place of --tag when it exists")
	cmd.MarkFlagsMutuallyExclusive("name", "tag")
	cmd.MarkFlagsOneRequired("name", "tag")
	return cmd
}

func newApplyMyCnfCmd() *cobra.Command {
	var (
		dataDir string
		file    string
	)
	cmd := &cobra.Command{
		Use:   "apply-my-cnf",
		Short: "Write spec.mysqlConfig into the my.cnf files of an existing data dir",
		Long: "Vitess writes my.cnf only when it first initialises a data dir, " +
			"and a restored snapshot carries the old file.\n" +
			"This command adds the settings of --file as a marked block at the end of every vt_*/my.cnf, and replaces that\n" +
			"block on every call. It does nothing on a data dir without my.cnf.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			extra, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			n, err := snapshot.ApplyMyCnf(dataDir, extra)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "my.cnf files changed: %d\n", n)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&dataDir, "data-dir", "/vt/vtdataroot", "vttestserver data dir")
	fl.StringVar(&file, "file", "", "option file with the [mysqld] settings to apply")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func newDeleteCmd() *cobra.Command {
	var (
		cf   commonFlags
		name string
	)
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a snapshot and the tags that point to it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithTermination(cf.terminationLog, func() error {
				if err := manifest.ValidateName(name); err != nil {
					return err
				}
				st, err := newStore(cmd.Context(), cf.s3)
				if err != nil {
					return err
				}
				return st.Delete(cmd.Context(), name)
			})
		},
	}
	cf.add(cmd)
	cmd.Flags().StringVar(&name, "name", "", "snapshot name")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
