COMPOSE := docker compose -f compose/compose.yaml --env-file compose/.env

.PHONY: env up down logs e2e chaos load seed live helm-deps helm-lint helm-install-dev

env:
	@test -f compose/.env || cp compose/.env.example compose/.env

up: env            ## build from ../proview-* and start the stack
	$(COMPOSE) up -d --build

down:              ## stop and remove volumes
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f --tail 100

e2e: env           ## end-to-end tests (starts the stack if needed)
	scripts/e2e.sh

chaos:             ## failure injection on the running stack
	scripts/chaos.sh

load:              ## k6 page-view load, e.g. make load RPS=5000 DURATION=2m
	docker run --rm --network host -v $(PWD)/load:/load grafana/k6:latest run \
	  -e RPS=$(or $(RPS),2000) -e DURATION=$(or $(DURATION),1m) /load/k6-pageviews.js

seed:              ## 14 days of demo history into ClickHouse
	cd tools && go run ./datagen seed -password $$(grep ^CLICKHOUSE_PASSWORD ../compose/.env | cut -d= -f2)

live:              ## live simulated traffic through the public APIs
	cd tools && go run ./datagen live -rps $(or $(RPS),50) -duration $(or $(DURATION),5m)

helm-deps:         ## package sibling service charts into helm/proview/charts
	scripts/helm-local-deps.sh

helm-lint: helm-deps
	helm lint helm/proview --strict
	helm lint helm/proview --strict -f helm/proview/values-dev.yaml
	helm lint helm/proview --strict -f helm/proview/values-prod.yaml

helm-install-dev: helm-deps   ## install into the current kube context (operators must be installed)
	helm upgrade --install proview helm/proview -n proview --create-namespace -f helm/proview/values-dev.yaml
