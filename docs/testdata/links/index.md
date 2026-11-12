# Test Links Fixture

This is a test file with various link types.

## Valid Links

- [Valid relative link](valid.md)
- [Valid subdir link](subdir/nested.md)
- [Valid parent link](../README.md) (but in testdata context this should fail)
- [Valid image link](images/test.png)
- [Link with anchor](valid.md#section)
- [Link with query](valid.md?param=1)

## Invalid Links

- [Missing file](missing.md)
- [Missing subdir file](subdir/nowhere.md)
- [Missing image](images/nonexistent.png)

## User Directory Paths

- [Home dir link](/Users/test/doc.md)
- [Linux home](/home/user/file.md)
- [Windows path](C:\Users\test\doc.md)

## External Links

- [External](https://example.com)
- [Mail](mailto:test@example.com)
