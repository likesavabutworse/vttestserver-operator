package vitess

import (
	"reflect"
	"testing"
)

const shardedVSchema = `{
  "sharded": true,
  "vindexes": {"xxhash": {"type": "xxhash"}},
  "tables": {
    "customer": {
      "column_vindexes": [{"column": "customer_id", "name": "xxhash"}],
      "auto_increment": {"column": "customer_id", "sequence": "customer_seq"}
    }
  }
}`

func TestParseVSchema(t *testing.T) {
	vs, err := ParseVSchema([]byte(shardedVSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !vs.Sharded || vs.Tables["customer"].AutoIncrement.Sequence != "customer_seq" {
		t.Fatalf("unexpected: %v", vs)
	}
	if _, err := ParseVSchema([]byte(`{"sharded": true, "tabels": {}}`)); err == nil {
		t.Fatal("a typo in a field name must be an error")
	}
	if _, err := ParseVSchema([]byte(`nope`)); err == nil {
		t.Fatal("want error for invalid JSON")
	}
	if _, err := ParseVSchema([]byte(`{}`)); err != nil {
		t.Fatalf("an empty VSchema is valid: %v", err)
	}
}

func TestVSchemaEqual(t *testing.T) {
	same, err := VSchemaEqual([]byte(shardedVSchema), []byte(`{"tables":{"customer":{"auto_increment":{"sequence":"customer_seq","column":"customer_id"},"column_vindexes":[{"name":"xxhash","column":"customer_id"}]}},"vindexes":{"xxhash":{"type":"xxhash"}},"sharded":true}`))
	if err != nil || !same {
		t.Fatalf("key order must not matter: %v %v", same, err)
	}
	same, err = VSchemaEqual([]byte(`{}`), []byte(shardedVSchema))
	if err != nil || same {
		t.Fatalf("different VSchemas: %v %v", same, err)
	}
}

func TestSplitStatements(t *testing.T) {
	sql := `-- sequences
create table if not exists customer_seq(id int, next_id bigint, cache bigint, primary key(id)) comment 'vitess_sequence';
insert into customer_seq(id, next_id, cache) values(0, 1000, 100) on duplicate key update id = id; # keep
create table if not exists t(a varchar(10) default 'x;y', b int) comment "semi;colon";
select 1`
	want := []string{
		"create table if not exists customer_seq(id int, next_id bigint, cache bigint, primary key(id)) comment 'vitess_sequence'",
		"insert into customer_seq(id, next_id, cache) values(0, 1000, 100) on duplicate key update id = id",
		`create table if not exists t(a varchar(10) default 'x;y', b int) comment "semi;colon"`,
		"select 1",
	}
	// The comment line at the start leaves a newline before the first statement.
	if got := SplitStatements(sql); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := SplitStatements("  ;; \n"); len(got) != 0 {
		t.Fatalf("empty input: %q", got)
	}
}
