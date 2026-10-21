# Marketplace — ДЗ №2

Go API с контрактным подходом: товары, заказы, промокоды, JWT и роли.

ДЗ №2 реализовано самостоятельным приложением. От первой работы сохранены Go, Docker и `/health`. Предыдущее решение находится в ветке `dz1`, его документация — в `docs/dz1/`.

## Запуск

Нужны Docker и Docker Compose. Для локальных команд разработки — Go 1.25+ и Make; для демонстрационного сценария — Python 3.

```bash
cp .env.example .env
docker compose up --build -d --wait
curl http://localhost:8080/health
```

Compose запускает PostgreSQL 17, применяет миграции **Flyway**, затем запускает API. Данные сохраняются в Docker volume. Порты API и PostgreSQL доступны только на localhost.

Начальная учётная запись администратора для локальной демонстрации: `admin@example.com` / `Admin-local-123!`. Значения задаются через `ADMIN_EMAIL` и `ADMIN_PASSWORD` в `.env`. При последующих стартах существующий пароль не перезаписывается. Через регистрацию доступны `USER` и `SELLER`.

```bash
docker compose logs -f api
docker compose logs migrate
docker compose down
```

## Контракт и кодогенерация

Источник контракта — [`api/openapi.yaml`](api/openapi.yaml). Работает также `GET http://localhost:8080/openapi.yaml` без авторизации.

```bash
make generate   # DTO, enums, интерфейс обработчиков, привязка маршрутов, embedded spec
make build      # сначала генерация, затем сборка bin/marketplace
make run        # генерация и запуск, DATABASE_URL и JWT_SECRET должны быть в окружении
```

Версия `oapi-codegen` закреплена в `go.mod`. Сгенерированный `internal/api/generated.go` находится в `.gitignore`. Docker тоже выполняет генерацию перед компиляцией. Обработчики реализуют сгенерированный `ServerInterface`; запросы и ответы используют сгенерированные типы. Ручных DTO нет.

Middleware `kin-openapi` проверяет JSON, обязательные поля, типы, enum, длины, диапазоны, массивы, query/path-параметры и лишние свойства **до вызова обработчика**. Ошибки содержат `error_code`, `message`, при необходимости `details` с полями и нарушениями.

## Методы и роли

Все методы, кроме `/auth/*`, `/health`, `/openapi.yaml`, требуют `Authorization: Bearer <access_token>`.

| Метод | Назначение | Доступ |
|---|---|---|
| `POST /auth/register` | Регистрация USER или SELLER | Публичный |
| `POST /auth/login` | Получение access и refresh JWT | Публичный |
| `POST /auth/refresh` | Одноразовый refresh с ротацией | Публичный |
| `GET /products` | Список, пагинация и фильтры | Все роли |
| `GET /products/{id}` | Товар по ID | Все роли |
| `POST /products` | Создать товар | SELLER — свой, ADMIN — любой |
| `PUT /products/{id}` | Полностью обновить товар | SELLER — свой, ADMIN — любой |
| `DELETE /products/{id}` | Перевести в ARCHIVED | SELLER — свой, ADMIN — любой |
| `POST /orders` | Создать заказ | USER, ADMIN |
| `GET /orders/{id}` | Получить заказ | USER — свой, ADMIN — любой |
| `PUT /orders/{id}` | Заменить позиции в CREATED | USER — свой, ADMIN — любой |
| `POST /orders/{id}/cancel` | Отменить CREATED/PAYMENT_PENDING | USER — свой, ADMIN — любой |
| `POST /orders/{id}/status` | Следующий статус | ADMIN |
| `POST /promo-codes` | Создать промокод | SELLER, ADMIN |

`GET /products?page=0&size=20&status=ACTIVE&category=books`: страницы с нуля, size по умолчанию 20, максимум 100, точное совпадение категории. Без фильтра status возвращаются все статусы, включая ARCHIVED. Сортировка по ID. Ответ: `items`, `totalElements`, `page`, `size`.

При создании товар принадлежит текущему продавцу. ADMIN может передать `seller_id` существующего SELLER/ADMIN. При обновлении владелец не меняется. Повторный DELETE успешен, запись остаётся в БД.

## Заказы и денежные расчёты

- Создание проверяет частоту → активный заказ → наличие/активность товаров → остатки → промокод. Неудачные запросы не записывают `user_operations`.
- Остатки резервируются при создании; цена фиксируется в `order_items.price_at_order`. Изменение каталожной цены не меняет созданный заказ.
- Обновление разрешено только в `CREATED`: возврат старых остатков, проверка и резерв новых, новые snapshots цен, пересчёт промокода и запись операции выполняются в одной транзакции. Ошибка откатывает всё.
- Повторяющиеся `product_id` в запросе отклоняются с 400. В заказе 1–50 позиций, в позиции 1–999 единиц.
- При отмене остатки и использование промокода возвращаются один раз. Повторная отмена возвращает 409.
- Деньги передаются как JSON number, хранятся как `DECIMAL(12,2)` и рассчитываются через `shopspring/decimal` без float64. Скидка округляется до копеек методом half-up.
- Процентная скидка ограничивается 70% суммы; фиксированная — суммой заказа. В неоднозначной формулировке задания про 70% принято именно ограничение скидки сверху.
- При обновлении промокод проверяется заново. Если он истёк, отключён или новая сумма ниже минимума, скидка снимается, FK обнуляется и `current_uses` уменьшается. Собственное уже учтённое использование не делает промокод исчерпанным при обновлении заказа.
- `orders.total_amount` — сумма **после** скидки, `discount_amount` — размер скидки. Превышение денежного диапазона отклоняется с 400.

Дополнительный ADMIN-метод `/orders/{id}/status` позволяет продемонстрировать полную модель состояний:

```text
CREATED → PAYMENT_PENDING → PAID → SHIPPED → COMPLETED
   ↓              ↓
CANCELED       CANCELED
```

Для отмены используется только `/cancel`. Недопустимые переходы возвращают `INVALID_STATE_TRANSITION`.

## Настройки

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `HTTP_PORT` | `8080` | Порт Compose на хосте |
| `DATABASE_URL` | В `.env.example` | Подключение при запуске вне Docker |
| `JWT_SECRET` | Локальный ключ в примере | Ключ HS256, минимум 32 байта |
| `ACCESS_TOKEN_TTL` | `20m` | От 15 до 30 минут |
| `REFRESH_TOKEN_TTL` | `336h` | От 7 до 30 дней |
| `ORDER_CREATE_INTERVAL` | `1m` | Интервал успешных созданий на пользователя |
| `ORDER_UPDATE_INTERVAL` | `1m` | Интервал успешных обновлений на владельца заказа |
| `ADMIN_EMAIL`, `ADMIN_PASSWORD` | Локальная учётная запись | Начальный администратор |

Refresh JWT хранится в БД в виде идентификатора `jti`, владельца, срока и признака отзыва; сам токен не хранится. Access и refresh имеют разные `aud` и `token_type`; алгоритм, подпись, issuer и срок проверяются. Повторное использование refresh отклоняется. Пароли хешируются bcrypt.

Каждый HTTP-запрос логируется в JSON: `request_id`, `method`, `endpoint`, `status_code`, `duration_ms`, `user_id`, `timestamp`. Для POST/PUT/DELETE добавляется тело с маскированием полей password/token/secret, в том числе вложенных. Невалидный JSON целиком не логируется. Сервер генерирует UUID и возвращает его в `X-Request-Id`.

## Проверка

```bash
make test         # unit-тесты, проверка контракта, race detector
make vet
make integration  # реальный PostgreSQL из запущенного Compose, race detector
make e2e          # HTTP-запросы и показ SELECT из БД
```

Для другой БД передайте `TEST_DATABASE_URL` при `make integration`. Схема должна быть применена Flyway. Интеграционные тесты и E2E создают отдельные уникальные записи и сохраняют их для просмотра; существующие записи не очищаются. E2E рассчитан на интервалы `1m` и локального администратора из `.env`.

Интеграционные тесты сверяют HTTP-ответы с OpenAPI, проверяют роли, ошибки, pagination/filtering, snapshots, rollback, лимиты, снятие скидки, возврат остатков, полную цепочку статусов, конкурентное списание последнего товара и конкурентное использование последнего промокода.

Показ таблиц вручную:

```bash
docker compose exec postgres psql -U marketplace -d marketplace
```

```sql
SELECT id, name, price, stock, status, seller_id FROM products ORDER BY id;
SELECT id, user_id, status, total_amount, discount_amount, promo_code_id FROM orders ORDER BY id;
SELECT order_id, product_id, quantity, price_at_order FROM order_items ORDER BY id;
SELECT code, current_uses, max_uses FROM promo_codes ORDER BY id;
SELECT user_id, operation_type, created_at FROM user_operations ORDER BY id;
SELECT installed_rank, version, description, success FROM flyway_schema_history;
```

Архитектура и блокировки: [`docs/architecture.md`](docs/architecture.md). План демонстрации и вопросы: [`docs/defense.md`](docs/defense.md).

Генерация: [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen). Валидация: [kin-openapi](https://github.com/getkin/kin-openapi). Миграции: [Flyway Docker](https://documentation.red-gate.com/flyway/reference/usage/flyway-docker).
