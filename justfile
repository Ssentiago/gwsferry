# показать список рецептов
default:
    @just --list

# собрать бинарник под linux amd64
build:
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o gwsferry-linux-amd64 ./cmd/gwsferry/

# запустить без сборки бинарника (для локальной разработки)
run:
    go run ./cmd/gwsferry/

# прогнать тесты
test:
    go test ./...

# тесты с покрытием
test-cover:
    go test -cover ./...

# линтер (golangci-lint)
lint:
    golangci-lint run

# форматирование
fmt:
    gofmt -w .

# собрать и запустить бинарник
start: build
    ./gwsferry-linux-amd64

# очистить артефакты сборки
clean:
    rm -f gwsferry-linux-amd64