# go-sqllexer

This repository contains a hand written SQL Lexer that tokenizes SQL queries with a focus on obfuscating and normalization. The lexer is written in Go with no external dependencies.
**Note** This is NOT a SQL parser, it only tokenizes SQL queries.

## Features

- :rocket: Fast and lightweight tokenization (not regex based)
- :lock: Obfuscates sensitive data (e.g. numbers, strings, specific literals like dollar quoted strings in Postgres, etc.)
- :book: Even works with truncated queries
- :globe_with_meridians: UTF-8 support
- :wrench: Normalizes obfuscated queries

## Installation

```bash
go get github.com/DataDog/go-sqllexer
```

## Usage

### Tokenize

```go
import "github.com/DataDog/go-sqllexer"

func main() {
    query := "SELECT * FROM users WHERE id = 1"
    lexer := sqllexer.New(query)
    tokens := lexer.ScanAll()
    for _, token := range tokens {
        fmt.Println(token)
    }
}
```

### Obfuscate

```go
import (
    "fmt"
    "github.com/DataDog/go-sqllexer"
)

func main() {
    query := "SELECT * FROM users WHERE id = 1"
    obfuscator := sqllexer.NewObfuscator()
    obfuscated := obfuscator.Obfuscate(query)
    // "SELECT * FROM users WHERE id = ?"
    fmt.Println(obfuscated)
}
```

### Normalize

```go
import (
    "fmt"
    "github.com/DataDog/go-sqllexer"
)

func main() {
    query := "SELECT * FROM users WHERE id in (?, ?)"
    normalizer := sqllexer.NewNormalizer(
        WithCollectComments(true),
        WithCollectCommands(true),
        WithCollectTables(true),
        WithKeepSQLAlias(false),
    )
    normalized, statementMetadata, err := normalizer.Normalize(query)
    // "SELECT * FROM users WHERE id in (?)"
    fmt.Println(normalized)
}
```

## Testing

```bash
go test -v ./...
```

## Benchmarks

```bash
go test -bench=. -benchmem ./...
```

## License

[MIT License](LICENSE)
