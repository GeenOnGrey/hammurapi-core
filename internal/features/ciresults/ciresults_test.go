package ciresults

import "testing"

const junit = `<?xml version="1.0"?>
<testsuites>
  <testsuite name="booking">
    <testcase classname="booking" name="TestQA03_weekend_slots" time="0.5"/>
    <testcase classname="booking" name="TestQA03_overlap" time="0.1"><failure message="x"/></testcase>
    <testcase classname="booking" name="test_qa_12 skipped" time="0"><skipped/></testcase>
    <testcase classname="booking" name="unrelated"/>
    <testcase classname="booking" name="tagged"><properties><property name="testcase" value="QA-01"/></properties></testcase>
  </testsuite>
</testsuites>`

// VAL-01: tests are linked to test cases by the ID in the name or a property.
func TestParseAndMatch(t *testing.T) {
	tests, err := ParseJUnit([]byte(junit))
	if err != nil || len(tests) != 5 {
		t.Fatalf("parse: %v %d", err, len(tests))
	}
	res := Match(tests, []string{"QA-01", "QA-03", "QA-1", "QA-12"})
	got := map[string]string{}
	for _, r := range res {
		got[r.TCID] = r.Status
	}
	want := map[string]string{"QA-03": "failed", "QA-12": "skipped", "QA-01": "passed"}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("%s: got %q want %q", id, got[id], st)
		}
	}
	if _, ok := got["QA-1"]; ok {
		t.Error("QA-1 must not match QA-12")
	}
}
