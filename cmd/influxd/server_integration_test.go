package main_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/influxdb/influxdb"
	"github.com/influxdb/influxdb/client"
	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/messaging"

	main "github.com/influxdb/influxdb/cmd/influxd"
)

// urlFor returns a URL with the path and query params correctly appended and set.
func urlFor(u *url.URL, path string, params url.Values) *url.URL {
	u.Path = path
	u.RawQuery = params.Encode()
	return u
}

// node represents a node under test, which is both a broker and data node.
type node struct {
	broker *messaging.Broker
	server *influxdb.Server
	url    *url.URL
	leader bool
}

// cluster represents a multi-node cluster.
type cluster []node

// createCombinedNodeCluster creates a cluster of nServers nodes, each of which
// runs as both a Broker and Data node. If any part cluster creation fails,
// the testing is marked as failed.
//
// This function returns a slice of nodes, the first of which will be the leader.
func createCombinedNodeCluster(t *testing.T, testName string, nNodes, basePort int) cluster {
	t.Logf("Creating cluster of %d nodes for test %s", nNodes, testName)
	if nNodes < 1 {
		t.Fatalf("Test %s: asked to create nonsense cluster", testName)
	}

	nodes := make([]node, 0)

	tmpDir := os.TempDir()
	tmpBrokerDir := filepath.Join(tmpDir, "broker-integration-test")
	tmpDataDir := filepath.Join(tmpDir, "data-integration-test")
	t.Logf("Test %s: using tmp directory %q for brokers\n", testName, tmpBrokerDir)
	t.Logf("Test %s: using tmp directory %q for data nodes\n", testName, tmpDataDir)
	// Sometimes if a test fails, it's because of a log.Fatal() in the program.
	// This prevents the defer from cleaning up directories.
	// To be safe, nuke them always before starting
	_ = os.RemoveAll(tmpBrokerDir)
	_ = os.RemoveAll(tmpDataDir)

	// Create the first node, special case.
	c := main.NewConfig()
	c.Broker.Dir = filepath.Join(tmpBrokerDir, strconv.Itoa(basePort))
	c.Data.Dir = filepath.Join(tmpDataDir, strconv.Itoa(basePort))
	c.Broker.Port = basePort
	c.Data.Port = basePort
	c.Admin.Enabled = false

	b, s := main.Run(c, "", "x.x", os.Stderr)
	if b == nil {
		t.Fatalf("Test %s: failed to create broker on port %d", testName, basePort)
	}
	if s == nil {
		t.Fatalf("Test %s: failed to create leader data node on port %d", testName, basePort)
	}
	nodes = append(nodes, node{
		broker: b,
		server: s,
		url:    &url.URL{Scheme: "http", Host: "localhost:" + strconv.Itoa(basePort)},
		leader: true,
	})

	// Create subsequent nodes, which join to first node.
	for i := 1; i < nNodes; i++ {
		nextPort := basePort + i
		c.Broker.Dir = filepath.Join(tmpBrokerDir, strconv.Itoa(nextPort))
		c.Data.Dir = filepath.Join(tmpDataDir, strconv.Itoa(nextPort))
		c.Broker.Port = nextPort
		c.Data.Port = nextPort

		b, s := main.Run(c, "http://localhost:"+strconv.Itoa(basePort), "x.x", os.Stderr)
		if b == nil {
			t.Fatalf("Test %s: failed to create following broker on port %d", testName, basePort)
		}
		if s == nil {
			t.Fatalf("Test %s: failed to create following data node on port %d", testName, basePort)
		}

		nodes = append(nodes, node{
			broker: b,
			server: s,
			url:    &url.URL{Scheme: "http", Host: "localhost:" + strconv.Itoa(nextPort)},
		})
	}

	return nodes
}

// createDatabase creates a database, and verifies that the creation was successful.
func createDatabase(t *testing.T, testName string, nodes cluster, database string) {
	t.Logf("Test: %s: creating database %s", testName, database)
	serverURL := nodes[0].url

	u := urlFor(serverURL, "query", url.Values{"q": []string{"CREATE DATABASE foo"}})
	resp, err := http.Get(u.String())
	if err != nil {
		t.Fatalf("Couldn't create database: %s", err)
	}
	defer resp.Body.Close()

	var results client.Results
	err = json.NewDecoder(resp.Body).Decode(&results)
	if err != nil {
		t.Fatalf("Couldn't decode results: %v", err)
	}

	if results.Error() != nil {
		t.Logf("results.Error(): %q", results.Error().Error())
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Create database failed.  Unexpected status code.  expected: %d, actual %d", http.StatusOK, resp.StatusCode)
	}

	if len(results.Results) != 1 {
		t.Fatalf("Create database failed.  Unexpected results length.  expected: %d, actual %d", 1, len(results.Results))
	}

	// Query the database exists
	u = urlFor(serverURL, "query", url.Values{"q": []string{"SHOW DATABASES"}})
	resp, err = http.Get(u.String())
	if err != nil {
		t.Fatalf("Couldn't query databases: %s", err)
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&results)
	if err != nil {
		t.Fatalf("Couldn't decode results: %v", err)
	}

	if results.Error() != nil {
		t.Logf("results.Error(): %q", results.Error().Error())
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("show databases failed.  Unexpected status code.  expected: %d, actual %d", http.StatusOK, resp.StatusCode)
	}

	expectedResults := client.Results{
		Results: []client.Result{
			{Rows: []influxql.Row{
				influxql.Row{
					Columns: []string{"name"},
					Values:  [][]interface{}{{"foo"}},
				},
			}},
		},
	}
	if !reflect.DeepEqual(results, expectedResults) {
		t.Fatalf("show databases failed.  Unexpected results.  expected: %+v, actual %+v", expectedResults, results)
	}
}

// createRetentionPolicy creates a retetention policy and verifies that the creation was successful.
func createRetentionPolicy(t *testing.T, testName string, nodes cluster, database, retention string) {
	t.Log("Creating retention policy")
	serverURL := nodes[0].url
	replication := fmt.Sprintf("CREATE RETENTION POLICY bar ON foo DURATION 1h REPLICATION %d DEFAULT", len(nodes))

	u := urlFor(serverURL, "query", url.Values{"q": []string{replication}})
	resp, err := http.Get(u.String())
	if err != nil {
		t.Fatalf("Couldn't create retention policy: %s", err)
	}
	defer resp.Body.Close()

	var results client.Results
	err = json.NewDecoder(resp.Body).Decode(&results)
	if err != nil {
		t.Fatalf("Couldn't decode results: %v", err)
	}

	if results.Error() != nil {
		t.Logf("results.Error(): %q", results.Error().Error())
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Create retention policy failed.  Unexpected status code.  expected: %d, actual %d", http.StatusOK, resp.StatusCode)
	}

	if len(results.Results) != 1 {
		t.Fatalf("Create retention policy failed.  Unexpected results length.  expected: %d, actual %d", 1, len(results.Results))
	}
}

// writes writes the provided data to the cluster. It verfies that a 200 OK is returned by the server.
func write(t *testing.T, testname string, nodes cluster, data string) {
	t.Logf("Test %s: writing data", testname)
	serverURL := nodes[0].url
	u := urlFor(serverURL, "write", url.Values{})

	buf := []byte(data)
	t.Logf("Writing raw data: %s", string(buf))
	resp, err := http.Post(u.String(), "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Couldn't write data: %s", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Write to database failed.  Unexpected status code.  expected: %d, actual %d", http.StatusOK, resp.StatusCode)
	}

	// Need some time for server to get consensus and write data
	// TODO corylanou query the status endpoint for the server and wait for the index to update to know the write was applied
	time.Sleep(time.Duration(len(nodes)) * time.Second)
}

// simpleQuery executes the given query against all nodes in the cluster, and verify the
// returned results are as expected.
func simpleQuery(t *testing.T, testname string, nodes cluster, query string, expected client.Results) {
	var results client.Results

	// Query the data exists
	for _, n := range nodes {
		t.Logf("Test name %s: query data on node %s", testname, n.url)
		u := urlFor(n.url, "query", url.Values{"q": []string{query}, "db": []string{"foo"}})
		resp, err := http.Get(u.String())
		if err != nil {
			t.Fatalf("Couldn't query databases: %s", err)
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Couldn't read body of response: %s", err)
		}
		t.Logf("resp.Body: %s\n", string(body))

		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		err = dec.Decode(&results)
		if err != nil {
			t.Fatalf("Couldn't decode results: %v", err)
		}

		if results.Error() != nil {
			t.Logf("results.Error(): %q", results.Error().Error())
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query databases failed.  Unexpected status code.  expected: %d, actual %d", http.StatusOK, resp.StatusCode)
		}

		if !reflect.DeepEqual(results, expected) {
			t.Logf("Expected: %#v\n", expected)
			t.Logf("Actual: %#v\n", results)
			t.Fatalf("query databases failed.  Unexpected results.")
		}
	}
}

func Test_ServerSingleIntegration(t *testing.T) {
	nNodes := 1
	basePort := 8090
	testName := "single node"
	now := time.Now().UTC()
	nodes := createCombinedNodeCluster(t, "single node", nNodes, basePort)

	createDatabase(t, testName, nodes, "foo")
	createRetentionPolicy(t, testName, nodes, "foo", "bar")
	write(t, testName, nodes, fmt.Sprintf(`
{
"database":
    "foo",
    "retentionPolicy":
    "bar",
    "points":
    [{
        "name":
        "cpu",
        "tags": {
            "host": "server01"
        },
        "timestamp": %d,
        "precision":"n",
        "values":{
            "value": 100
        }
    }]
}
`, now.UnixNano()))
	expectedResults := client.Results{
		Results: []client.Result{
			{Rows: []influxql.Row{
				{
					Name:    "cpu",
					Columns: []string{"time", "value"},
					Values: [][]interface{}{
						[]interface{}{now.Format(time.RFC3339Nano), json.Number("100")},
					},
				}}},
		},
	}

	simpleQuery(t, testName, nodes[:1], `select value from "foo"."bar".cpu`, expectedResults)
}

func Test_Server3NodeIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	nNodes := 3
	basePort := 8190
	testName := "3 node"
	now := time.Now().UTC()
	nodes := createCombinedNodeCluster(t, testName, nNodes, basePort)

	createDatabase(t, testName, nodes, "foo")
	createRetentionPolicy(t, testName, nodes, "foo", "bar")
	write(t, testName, nodes, fmt.Sprintf(`
{
"database":
	"foo",
	"retentionPolicy":
	"bar",
	"points":
	[{
		"name":
		"cpu",
		"tags": {
			"host": "server01"
		},
		"timestamp": %d,
		"precision":"n",
		"values":{
			"value": 100
		}
	}]
}
`, now.UnixNano()))
	expectedResults := client.Results{
		Results: []client.Result{
			{Rows: []influxql.Row{
				{
					Name:    "cpu",
					Columns: []string{"time", "value"},
					Values: [][]interface{}{
						[]interface{}{now.Format(time.RFC3339Nano), json.Number("100")},
					},
				}}},
		},
	}

	simpleQuery(t, testName, nodes[:1], `select value from "foo"."bar".cpu`, expectedResults)
}

func Test_Server5NodeIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	nNodes := 5
	basePort := 8290
	testName := "5 node"
	now := time.Now().UTC()
	nodes := createCombinedNodeCluster(t, testName, nNodes, basePort)

	createDatabase(t, testName, nodes, "foo")
	createRetentionPolicy(t, testName, nodes, "foo", "bar")
	write(t, testName, nodes, fmt.Sprintf(`
{
"database":
    "foo",
    "retentionPolicy":
    "bar",
    "points":
    [{
        "name":
        "cpu",
        "tags": {
            "host": "server01"
        },
        "timestamp": %d,
        "precision":"n",
        "values":{
            "value": 100
        }
    }]
}
`, now.UnixNano()))

	expectedResults := client.Results{
		Results: []client.Result{
			{Rows: []influxql.Row{
				{
					Name:    "cpu",
					Columns: []string{"time", "value"},
					Values: [][]interface{}{
						[]interface{}{now.Format(time.RFC3339Nano), json.Number("100")},
					},
				}}},
		},
	}

	simpleQuery(t, testName, nodes[:1], `select value from "foo"."bar".cpu`, expectedResults)
}

func Test_AllTagCombinationsInShowSeriesAreSelectable(t *testing.T) {
	nNodes := 1
	basePort := 8390
	testName := "all tag combinations in SHOW SERIES are selectable"
	now := time.Now().UTC()
	nodes := createCombinedNodeCluster(t, testName, nNodes, basePort)

	createDatabase(t, testName, nodes, "foo")
	createRetentionPolicy(t, testName, nodes, "foo", "bar")

	bp := influxdb.BatchPoints{
		Database: "foo",
	}

	measurementName := "my_measurement"

	// 21 points, 3 tagRarity results in this test failing ~100% of the time
	// this test seems to fail when a tag is associated with < 30% of total points
	totalPoints := 21
	tagRarity := 3
	for i := 0; i < totalPoints; i++ {
		var rarity string
		if (i % tagRarity) == 0 {
			rarity = "rare"
		} else {
			rarity = "common"
		}

		p := client.Point{
			Name: measurementName,
			Tags: map[string]string{
				"host":   fmt.Sprintf("server%d", i%10),
				"rarity": rarity,
			},
			Values: map[string]interface{}{
				"value": float64(i) / 1000.0,
			},
			Timestamp: client.Timestamp(now.Add(time.Duration(-i) * time.Second)),
		}

		bp.Points = append(bp.Points, p)
	}

	bpJson, err := json.Marshal(bp)
	if err != nil {
		t.Fatalf("Expected no error, received %v", err)
	}

	write(t, testName, nodes, string(bpJson))

	c, err := client.NewClient(client.Config{URL: *nodes[0].url})
	if err != nil {
		t.Fatalf("Expected no error, received %v", err)
	}

	result, err := c.Query(client.Query{Command: "SHOW SERIES", Database: "foo"})
	if err != nil {
		t.Fatalf("Expected no error, received %v", err)
	}

	var tagPairs [][]string
	for _, r := range result.Results {
		for _, row := range r.Rows {
			for _, tagPair := range row.Values {
				pair := []string{}
				for _, tag := range tagPair {
					pair = append(pair, tag.(string))
				}
				tagPairs = append(tagPairs, pair)
			}
		}
	}

	for _, tagPair := range tagPairs {
		host := tagPair[0]
		rarity := tagPair[1]
		query := fmt.Sprintf("SELECT value FROM %s WHERE host = '%s' AND rarity = '%s'", measurementName, host, rarity)
		result, err = c.Query(client.Query{Command: query, Database: "foo"})
		if err != nil {
			t.Fatalf("Expected no error, received %v", err)
		}
		if result.Err != nil {
			t.Fatalf("Expected no error, received %v", result.Err)
		}
	}
}