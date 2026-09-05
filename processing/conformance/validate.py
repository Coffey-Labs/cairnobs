#!/usr/bin/env python3
"""Structural check for the processing conformance corpus.

The cases are the specification (see ../README.md), so they have to stay
well-formed even while neither implementation exists to execute them.
This validates shape only -- it does not run rules against records,
because there is nothing yet to run them with.

Stdlib only, on purpose: this must be runnable anywhere without a
dependency install, including from a bare CI container.

Exit 0 if every case is valid, 1 otherwise.
"""

import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
CASES = os.path.join(HERE, "cases")

CASE_KEYS = {"name", "description", "rules", "inputs", "expect"}
RECORD_KEYS = {"timestamp_unix_nano", "host", "service", "severity",
               "message", "attributes"}

SEVERITIES = {"SEVERITY_UNSPECIFIED", "SEVERITY_TRACE", "SEVERITY_DEBUG",
              "SEVERITY_INFO", "SEVERITY_WARN", "SEVERITY_ERROR",
              "SEVERITY_FATAL"}

OPS_WITH_VALUE = {"eq", "ne", "contains", "prefix", "suffix", "regex"}
OPS_WITHOUT_VALUE = {"exists", "not_exists"}

TOP_LEVEL_FIELDS = {"message", "host", "service", "severity"}

# action -> (required params, optional params)
ACTIONS = {
    "drop":                (set(), set()),
    "drop_fields":         ({"fields"}, set()),
    "keep_fields":         ({"fields"}, set()),
    "rename":              ({"from", "to"}, set()),
    "derive":              ({"field"}, {"value", "from_field"}),
    "mask":                ({"field", "pattern", "replacement"}, set()),
    "parse_json":          ({"field"}, {"prefix"}),
    "parse_regex":         ({"field", "pattern"}, set()),
    "sample":              ({"keep_one_in"}, set()),
    "suppress_duplicates": ({"window_ms"}, {"key_fields"}),
}

# Deliberately absent from ACTIONS. The design does not say what it emits
# or what a query that is not expecting a synthetic record sees, so a
# case using it would invent that answer and freeze it by accident.
UNSPECIFIED_ACTIONS = {"aggregate_count"}


def valid_field(name):
    return name in TOP_LEVEL_FIELDS or name.startswith("attributes.")


def check_record(rec, where, err):
    if not isinstance(rec, dict):
        return err(f"{where}: record must be an object")
    missing = RECORD_KEYS - set(rec)
    unknown = set(rec) - RECORD_KEYS
    if missing:
        err(f"{where}: record missing {sorted(missing)}")
    if "record_id" in unknown:
        err(f"{where}: record_id must never appear -- no case may depend "
            f"on it (see ../README.md)")
    if unknown - {"record_id"}:
        err(f"{where}: record has unknown keys {sorted(unknown - {'record_id'})}")
    if rec.get("severity") not in SEVERITIES:
        err(f"{where}: unknown severity {rec.get('severity')!r}")
    if not isinstance(rec.get("timestamp_unix_nano"), int):
        err(f"{where}: timestamp_unix_nano must be an integer")
    attrs = rec.get("attributes")
    if not isinstance(attrs, dict):
        err(f"{where}: attributes must be an object")
    else:
        for k, v in attrs.items():
            if not isinstance(k, str) or not isinstance(v, str):
                err(f"{where}: attributes must be string->string, got {k!r}={v!r}")


def check_action(a, where, err):
    if not isinstance(a, dict) or "action" not in a:
        return err(f"{where}: action must be an object with an 'action' key")
    name = a["action"]
    if name in UNSPECIFIED_ACTIONS:
        return err(f"{where}: '{name}' is deliberately unspecified -- decide "
                   f"its output shape in the design doc before writing cases "
                   f"for it")
    if name not in ACTIONS:
        return err(f"{where}: unknown action {name!r}")
    required, optional = ACTIONS[name]
    given = set(a) - {"action"}
    if required - given:
        err(f"{where}: action {name!r} missing {sorted(required - given)}")
    if given - required - optional:
        err(f"{where}: action {name!r} has unknown params "
            f"{sorted(given - required - optional)}")
    if name == "derive" and not ({"value", "from_field"} & given):
        err(f"{where}: derive needs exactly one of value/from_field")
    if name == "derive" and {"value", "from_field"} <= given:
        err(f"{where}: derive needs exactly one of value/from_field, not both")
    for key in ("field", "from", "to"):
        if key in a and not valid_field(a[key]):
            err(f"{where}: {key}={a[key]!r} is not an addressable field")
    for key in ("fields", "key_fields"):
        for f in a.get(key, []):
            if not valid_field(f):
                err(f"{where}: {key} entry {f!r} is not an addressable field")
    for key in ("pattern",):
        if key in a:
            try:
                re.compile(a[key])
            except re.error as exc:
                err(f"{where}: {key} is not a valid regex: {exc}")
    if name == "sample":
        n = a.get("keep_one_in")
        if not isinstance(n, int) or n < 1:
            err(f"{where}: keep_one_in must be an integer >= 1")
    if name == "suppress_duplicates":
        w = a.get("window_ms")
        if not isinstance(w, int) or w < 1:
            err(f"{where}: window_ms must be an integer >= 1")


def check_case(path, err):
    with open(path) as fh:
        try:
            case = json.load(fh)
        except json.JSONDecodeError as exc:
            return err(f"invalid JSON: {exc}")

    missing = CASE_KEYS - set(case)
    unknown = set(case) - CASE_KEYS
    if missing:
        err(f"missing top-level keys {sorted(missing)}")
    if unknown:
        err(f"unknown top-level keys {sorted(unknown)}")

    expected_name = os.path.basename(path)[:-len(".json")]
    if case.get("name") != expected_name:
        err(f"name {case.get('name')!r} does not match filename "
            f"{expected_name!r}")
    if not case.get("description"):
        err("description must not be empty -- it is what the case is for")

    for i, rule in enumerate(case.get("rules", [])):
        where = f"rules[{i}]"
        if not isinstance(rule, dict) or set(rule) != {"match", "actions"}:
            err(f"{where}: a rule is exactly {{match, actions}}")
            continue
        for j, clause in enumerate(rule["match"]):
            cw = f"{where}.match[{j}]"
            if not isinstance(clause, dict):
                err(f"{cw}: clause must be an object")
                continue
            op = clause.get("op")
            if op in OPS_WITHOUT_VALUE:
                if "value" in clause:
                    err(f"{cw}: {op} takes no value")
                allowed = {"field", "op"}
            elif op in OPS_WITH_VALUE:
                if "value" not in clause:
                    err(f"{cw}: {op} requires a value")
                allowed = {"field", "op", "value"}
            else:
                err(f"{cw}: unknown op {op!r}")
                continue
            if set(clause) - allowed:
                err(f"{cw}: unknown keys {sorted(set(clause) - allowed)}")
            if not valid_field(clause.get("field", "")):
                err(f"{cw}: {clause.get('field')!r} is not an addressable field")
            if op == "regex":
                try:
                    re.compile(clause["value"])
                except re.error as exc:
                    err(f"{cw}: not a valid regex: {exc}")
        if not rule["actions"]:
            err(f"{where}: a rule with no actions does nothing")
        for j, action in enumerate(rule["actions"]):
            check_action(action, f"{where}.actions[{j}]", err)

    for i, rec in enumerate(case.get("inputs", [])):
        check_record(rec, f"inputs[{i}]", err)
    if not case.get("inputs"):
        err("inputs must not be empty -- a case with no input asserts nothing")
    for i, rec in enumerate(case.get("expect", [])):
        check_record(rec, f"expect[{i}]", err)


def main():
    if not os.path.isdir(CASES):
        print(f"no cases directory at {CASES}", file=sys.stderr)
        return 1
    files = sorted(f for f in os.listdir(CASES) if f.endswith(".json"))
    if not files:
        print("no cases found", file=sys.stderr)
        return 1

    failures = 0
    for name in files:
        errors = []
        check_case(os.path.join(CASES, name), errors.append)
        if errors:
            failures += 1
            print(f"FAIL {name}")
            for e in errors:
                print(f"       {e}")
    print(f"\n{len(files) - failures}/{len(files)} cases valid")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
