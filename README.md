# proview-deploy

Доставка и проверка платформы **ProView** целиком: локальный стек в Docker Compose, umbrella Helm-чарт для Kubernetes с инфраструктурой через операторы, сквозные (e2e), chaos- и нагрузочные тесты, генератор демо-данных.

Документация платформы: [proview-docs](https://github.com/DemonIngvi/proview-docs).

| Репозиторий | Роль |
|---|---|
| [proview-banner-service](https://github.com/DemonIngvi/proview-banner-service) | выдача баннера из пула по преднастройке места |
| [proview-event-collector](https://github.com/DemonIngvi/proview-event-collector) | приём событий из браузера → Kafka |
| [proview-event-ingester](https://github.com/DemonIngvi/proview-event-ingester) | Kafka → ClickHouse, схема и агрегаты |
| [proview-analytics](https://github.com/DemonIngvi/proview-analytics) | API срезов и админка с визуализацией |
| [proview-web](https://github.com/DemonIngvi/proview-web) | демо-страница и JS SDK |

## Быстрый старт (Docker Compose)

Нужны Docker с Compose v2 и репозитории сервисов рядом с этим:

```bash
mkdir proview && cd proview
for r in proview-deploy proview-banner-service proview-event-collector proview-event-ingester proview-analytics proview-web; do
  git clone https://github.com/DemonIngvi/$r.git
done
cd proview-deploy
make up          # сборка всех образов и запуск
make seed        # 14 дней истории для дашборда (нужен Go) — необязательно
```

| Адрес | Что |
|---|---|
| http://localhost:8080 | демо-сайт с тремя рекламными местами (`?debug=1` — лента событий) |
| http://localhost:8090 | админка аналитики (`admin` / `ADMIN_PASSWORD` из `compose/.env`) |
| http://localhost:8081/admin/v1/banners | API управления пулом (`Authorization: Bearer $BANNER_ADMIN_TOKEN`) |
| http://localhost:8123 | ClickHouse HTTP (только localhost) |

Без исходников можно поднять опубликованные образы: `PROVIEW_TAG=0.1.0 docker compose -f compose/compose.yaml up -d --no-build`.

## Тесты

| Команда | Что проверяет |
|---|---|
| `make e2e` | путь браузер → баннер → события → Kafka → ClickHouse → API аналитики; таргетинг по стране через API управления; отказ поддельных токенов |
| `make chaos` | остановка ClickHouse и `SIGKILL` инжестера под нагрузкой: события доставляются **ровно один раз** после восстановления |
| `make load RPS=20000 DURATION=10m` | k6: просмотр страницы = `GET /v1/banner` + `POST /v1/events`; пороги p95 < 50 мс, ошибки < 1 % |

Замер на одном хосте (16 vCPU, весь стек и k6 на одной машине): 6,8k RPS без ошибок, p99 3 мс. При цели 20k хост держит ~17k RPS: 0,005 % ошибок, p95 148 мс, упор в CPU хоста. Инжест успевает: 1,95 млн событий за 70 с, лаг потребителя 0. Расчёт ёмкости кластера под 20k RPS — в `proview-docs/docs/architecture.md`.

## Kubernetes (Helm)

Чарт `helm/proview` подключает чарты сервисов из `oci://ghcr.io/demoningvi/charts` и описывает инфраструктуру ресурсами операторов:

| Компонент | Оператор | По умолчанию |
|---|---|---|
| Kafka (KRaft) | Strimzi | 3 узла, RF 3, `min.insync.replicas` 2, топик 24 партиции, хранение 7 дней |
| PostgreSQL | CloudNativePG | 3 инстанса, синхронная реплика, автоматический failover |
| ClickHouse + Keeper | Altinity clickhouse-operator | 1 шард × 3 реплики, `Replicated`-БД, Keeper × 3, пользователи `proview` и `proview_ro` |

```bash
# один раз на кластер (команды установки операторов — proview-docs/docs/runbooks/install.md)
helm upgrade --install proview oci://ghcr.io/demoningvi/charts/proview -n proview --create-namespace \
  -f values-prod.yaml
# из исходников
make helm-install-dev
```

Профили: `values-dev.yaml` (один узел, без автомасштабирования) и `values-prod.yaml` (секреты из внешнего хранилища, TLS-ingress, ServiceMonitor, NetworkPolicy, KEDA по лагу Kafka).

## Структура

```
compose/        стек для разработки и e2e (+ .env.example, пользователь ClickHouse только для чтения)
helm/proview/   umbrella-чарт: зависимости + CR Strimzi / CNPG / Altinity, профили dev/prod
e2e/            Go-тесты (-tags e2e), включая chaos-фазы
load/           сценарий k6
tools/datagen   seed (история в ClickHouse) и live (трафик через публичные API)
scripts/        e2e.sh, chaos.sh, helm-local-deps.sh
```

Лицензия: Apache-2.0.
