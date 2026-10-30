// Package vitess is a thin wrapper over the Vitess vtctld gRPC client. The
// operator uses it to apply a VSchema and init SQL to a running vttestserver.
package vitess

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/vt/grpcclient"
	vschemapb "vitess.io/vitess/go/vt/proto/vschema"
	vtctldatapb "vitess.io/vitess/go/vt/proto/vtctldata"
	"vitess.io/vitess/go/vt/vtctl/grpcvtctldclient"
	"vitess.io/vitess/go/vt/vtctl/vtctldclient"
)

// Client talks to a vttestserver: the vtctld gRPC port (PORT+1) for the VSchema,
// and the vtgate MySQL port (PORT+3) for SQL.
type Client struct {
	c         vtctldclient.VtctldClient
	mysqlHost string
	mysqlPort int
}

// Dial connects to the gRPC address and records the MySQL address. vttestserver has
// no authentication on either port by default. The MySQL connection opens per ExecSQL call.
func Dial(ctx context.Context, grpcAddr, mysqlAddr string) (*Client, error) {
	host, portStr, err := net.SplitHostPort(mysqlAddr)
	if err != nil {
		return nil, fmt.Errorf("mysql address %q: %w", mysqlAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("mysql address %q: %w", mysqlAddr, err)
	}
	c, err := grpcvtctldclient.NewWithDialOpts(ctx, grpcAddr, grpcclient.FailFast(false),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial vtctld %s: %w", grpcAddr, err)
	}
	return &Client{c: c, mysqlHost: host, mysqlPort: port}, nil
}

func (c *Client) Close() error { return c.c.Close() }

// ParseVSchema reads the JSON form of a keyspace VSchema, the same JSON that
// `vtctldclient GetVSchema` prints. It rejects unknown fields.
func ParseVSchema(data []byte) (*vschemapb.Keyspace, error) {
	vs := &vschemapb.Keyspace{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, vs); err != nil {
		return nil, fmt.Errorf("invalid VSchema JSON: %w", err)
	}
	return vs, nil
}

// ApplyVSchema replaces the VSchema of a keyspace. Vitess applies it as a full
// replace, so calling it twice with the same input changes nothing.
func (c *Client) ApplyVSchema(ctx context.Context, keyspace string, vschemaJSON []byte) error {
	vs, err := ParseVSchema(vschemaJSON)
	if err != nil {
		return fmt.Errorf("keyspace %s: %w", keyspace, err)
	}
	if _, err := c.c.ApplyVSchema(ctx, &vtctldatapb.ApplyVSchemaRequest{Keyspace: keyspace, VSchema: vs}); err != nil {
		return fmt.Errorf("apply VSchema to keyspace %s: %w", keyspace, err)
	}
	return nil
}

// GetVSchema returns the VSchema of a keyspace as JSON.
func (c *Client) GetVSchema(ctx context.Context, keyspace string) ([]byte, error) {
	resp, err := c.c.GetVSchema(ctx, &vtctldatapb.GetVSchemaRequest{Keyspace: keyspace})
	if err != nil {
		return nil, fmt.Errorf("get VSchema of keyspace %s: %w", keyspace, err)
	}
	return protojson.Marshal(resp.VSchema)
}

// VSchemaEqual reports whether two VSchema JSON documents hold the same VSchema.
// Defaults such as an absent "sharded: false" do not count as a difference.
func VSchemaEqual(a, b []byte) (bool, error) {
	va, err := ParseVSchema(a)
	if err != nil {
		return false, err
	}
	vb, err := ParseVSchema(b)
	if err != nil {
		return false, err
	}
	na, err := normalize(va)
	if err != nil {
		return false, err
	}
	nb, err := normalize(vb)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(na, nb), nil
}

func normalize(vs *vschemapb.Keyspace) (any, error) {
	b, err := protojson.Marshal(vs)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ExecSQL runs statements in a keyspace through vtgate. vtctld ApplySchema is not
// implemented in vtcombo, so vttestserver needs the MySQL protocol for DDL. Each
// statement must be idempotent, because the operator runs them again after a pod restart.
func (c *Client) ExecSQL(ctx context.Context, keyspace string, statements []string) error {
	if len(statements) == 0 {
		return nil
	}
	conn, err := mysql.Connect(ctx, &mysql.ConnParams{
		Host: c.mysqlHost, Port: c.mysqlPort, Uname: "root", DbName: keyspace,
	})
	if err != nil {
		return fmt.Errorf("connect to vtgate for keyspace %s: %w", keyspace, err)
	}
	defer conn.Close()
	for _, stmt := range statements {
		if _, err := conn.ExecuteFetch(stmt, 10, false); err != nil {
			return fmt.Errorf("keyspace %s: %w (statement: %s)", keyspace, err, truncate(stmt, 120))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// WaitReady polls GetKeyspaces until the topology answers or the timeout ends.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if _, err := c.c.GetKeyspaces(ctx, &vtctldatapb.GetKeyspacesRequest{}); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return fmt.Errorf("vtctld not ready: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// SplitStatements splits a SQL file into statements on semicolons that end a
// line or the file. It ignores semicolons in quotes and in "--" and "#" comments.
// Init SQL is simple DDL, so this is enough. It does not handle DELIMITER.
func SplitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	rs := []rune(sql)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == '\\' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
			cur.WriteRune(r)
		case r == '#' || (r == '-' && i+1 < len(rs) && rs[i+1] == '-'):
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			cur.WriteRune('\n')
		case r == ';':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
