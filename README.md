# legitpayz-customer-service (legitPayz)

Customer onboarding + KYC + login. (port `8001`). Part of the legitPayz credit-card microservices system.

## Run locally

Prereqs: Go 1.26+, Postgres (`cc/creditcard` migrated with `migrations/001_init.sql`), Redis.

```bash
PORT=8001 go run .
curl localhost:8001/health
```

## Docker

```bash
docker build -t legitpayz-customer-service .
docker run -p 8001:8001 legitpayz-customer-service
```

## Env

| Var | Purpose |
|-----|---------|
`DATABASE_URL` Postgres DSN · `REDIS_ADDR` · `AWS_ENDPOINT_URL` Floci/SQS endpoint · `KYC_QUEUE_URL` · `KYC_BUCKET` · `SNS_TOPIC_ARN`
