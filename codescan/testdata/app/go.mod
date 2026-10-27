module example.com/app

go 1.22

require (
	github.com/go-chi/chi/v5 v5.0.0
	github.com/redis/go-redis/v9 v9.0.0
	go.uber.org/fx v1.0.0
)

replace go.uber.org/fx => ./_stubs/fx

replace github.com/go-chi/chi/v5 => ./_stubs/chi

replace github.com/redis/go-redis/v9 => ./_stubs/redis
