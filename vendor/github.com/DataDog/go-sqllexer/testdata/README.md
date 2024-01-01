# Test Suite

The test suite is a collection of test SQL statements that are organized per DBMS. The test suite is used to test the SQL obfuscator and normalizer for correctness and completeness. It is also intended to cover DBMS specific edge cases, that are not covered by the generic unit tests.

## Test Suite Structure

The test suite is organized in the following way:

```text
testdata
├── README.md
├── dbms1
│   ├── query_type1
│   │   ├── test1.json
│   └── query_type2
│       ├── test1.json
dbms_test.go
```

The test suite is organized per DBMS. Each DBMS has a number of query types. Each query type has a number of test cases. Each test case consists of a SQL statement and the expected output of the obfuscator/normalizer.

## Test File Format

The test files are simple json files where each test case comes with one input SQL statements and an array of expected outputs.
Each expected output can optionally come with a configuration for the obfuscator and normalizer. The configuration is optional, because the default configuration is used if no configuration is provided.

testcase.json:

```json
{
    "input": "SELECT * FROM table1",
    "outputs": [
        {
            // Test case 1
            "expected": "SELECT * FROM table1",
            "obfuscator_config": {...}, // optional
            "normalizer_config": {...}  // optional
        },
        {
            // Test case 2
            "expected": "SELECT * FROM table1",
            "obfuscator_config": {...}, // optional
            "normalizer_config": {...}  // optional
        }
    ]
}
```

## How to write a new test case

1. Create a new directory for the DBMS, if it does not exist yet. (this step is often not necessary)
2. Create a new directory for the query type, if it does not exist yet.
3. Create a new test case `.json` file with the SQL statement and expected output. Refer to the [test file format](#test-file-format) or `testcase struct` in [dbms_test.go](../dbms_test.go) for more details.
4. Run the test suite to verify that the test case is working as expected.
