# FamilyChat Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans for inline execution or superpowers:subagent-driven-development if the user selects delegation. Track each completed step with checkboxes.

**Goal:** Реализовать Go-сервер домашнего мессенджера и React-админку с серверной историей, защищёнными S3-вложениями и звонками один на один.

**Architecture:** Один API-процесс обслуживает REST, WebSocket и статику админки. PostgreSQL хранит пользователей, сессии, чаты, сообщения, события и задания; приватный S3 хранит вложения. Worker создаёт превью и удаляет незавершённые загрузки, coturn обслуживает WebRTC relay.

**Tech Stack:** Go, net/http, pgx/v5, AWS SDK for Go v2, coder/websocket, Argon2id, PostgreSQL, FFmpeg, React, TypeScript, Vite, Vitest, Playwright, Docker Compose. Конкретные версии закрепить при установке и проверить по официальной документации.

**Spec:** `server/ARCHITECTURE.md`.

## Global Constraints

- Вся реализация находится в `server`; каталог `mobile` не изменяется.
- Пользователь самостоятельно создаёт аккаунт с логином и паролем, если администратор разрешил регистрацию.
- Сервер — Go, административная панель — React; звонки только между двумя пользователями.
- REST и пользовательские файлы требуют `Authorization: Bearer <session_token>`.
- Каждый `GET`, `HEAD`, условный и `Range`-запрос файла проверяет текущую сессию и права.
- Сервер не выдаёт клиенту публичные/подписанные ссылки S3 или токены доступа к файлам в URL.
- Первоначальные лимиты: 250 МиБ на файл, 10 ГиБ на пользователя; оба настраиваются.
- Администратор не получает доступ к чужим чатам через административную роль.
- Таймаут ответа на звонок — 45 секунд; окно восстановления сигнализации — 30 секунд.
- Неисполненные live-проверки нельзя заменять успешной сборкой или тестами сигнализации.
- Секреты и пользовательское содержимое не попадают в Git, логи или скриншоты проверок.

## Рабочие решения для реализации

- В `server` пока отсутствуют Go-модуль, приложение и Git-репозиторий. Перед кодом создать
  Git-репозиторий в `server` и рабочую ветку `feature/initial-server`; не менять соседние каталоги.
- Модуль `familychat/server`; корневой команды `go test ./...` достаточно, `go.work` не требуется.
- Сессия имеет абсолютный срок 30 дней, настраиваемый `SESSION_TTL`; отдельного refresh-токена
  в первой версии нет. При истечении клиент повторяет вход. Токен — 32 случайных байта,
  base64url без padding; БД хранит SHA-256. Билет WebSocket живёт 30 секунд и используется один раз.
- Новый пароль содержит 12–128 Unicode-символов; HTTP-ввод ограничен и по байтам.
  Логин: 3–32 символа ASCII `[A-Za-z0-9_]`, нормализация в нижний регистр.
- API принимает JSON с ограничением 1 МиБ; текст сообщения — до 16 000 символов,
  до 10 вложений на сообщение. История: 50 элементов по умолчанию, максимум 100.
- Регистрация и вход ограничены по IP и логину; IP из forwarded-заголовков доверяется
  только при явно настроенных доверенных proxy.
- Отсутствующая или отозванная сессия даёт `401`, недоступный объект — `404`,
  недостаточная административная роль — `403`, конфликт состояния — `409`.
- TURN-реквизиты живут 10 минут и обновляются клиентом до истечения, пока звонок активен.
- Поддерживаемые превью первой версии: JPEG, PNG, WebP, GIF (статический кадр),
  MP4 и WebM при наличии поддерживаемого видеокодека. Остальные форматы — обычные файлы.
- Интеграционные тесты используют отдельные БД и бакет с префиксом `familychat-test-`;
  очистка тестовых данных ограничена этими ресурсами.
- Общая инструкция `cargo check --workspace` неприменима к выбранному пользователем Go:
  использовать `gofmt`, `go vet`, `go test -race`, `go build`.

## Review Focus

1. Конкурирующие запросы не создают второй личный чат, дубли сообщения или два активных звонка (задачи 3, 4, 8).
2. Отзыв доступа между двумя запросами не позволяет прочитать следующий диапазон или получить WebSocket-событие (задачи 2, 5, 6).
3. Обрыв загрузки и перезапуск worker не теряют квоту навсегда и не публикуют частичный объект (задачи 6, 7).
4. Задержанный HTTP-ответ после logout не возвращает данные предыдущей сессии в React-панель (задача 9).
5. Успешный обмен SDP без реального медиапотока не считается работающим звонком (задача 10).

## Файлы и общие интерфейсы

- `cmd/familychat/main.go`: команды `serve`, `worker`, `migrate`, `bootstrap-admin`.
- `internal/config/config.go`: `Load() (Config, error)`; проверенные настройки запуска.
- `internal/database/database.go`: `Open(ctx context.Context, url string) (*pgxpool.Pool, error)`.
- `internal/database/migrations/*.sql`: встроенные миграции; `Migrate(ctx, pool) error` с блокировкой.
- `internal/auth/auth.go`: `Service`, `Principal{UserID, SessionID, Role string}`;
  `Authenticate(ctx context.Context, token string) (Principal, error)`,
  `Require(next http.Handler) http.Handler`, `PrincipalFrom(ctx context.Context) (Principal, bool)`.
- `internal/httpapi/server.go`: `Dependencies{Config, DB, Auth, Objects, Events}` и
  `New(deps Dependencies) http.Handler`; feature-файлы регистрируют пути через `http.ServeMux`.
- `internal/events/hub.go`: `Hub`, `CloseSession(sessionID string)`, `CloseUser(userID string)`;
  события всегда содержат `id`, `type`, `chat_id`, `occurred_at`, `data`.
- `internal/objects/s3.go`: `Store` с `Put(ctx, key, reader, size, contentType) error`,
  `Head(ctx, key) (Metadata, error)`, `Get(ctx, key, byteRange) (Object, error)`,
  `Delete(ctx, key) error`. `Metadata` содержит размер и MIME; `Object` — метаданные,
  `io.ReadCloser`, HTTP-статус и `ContentRange`. Ключи генерирует сервер.
- `internal/worker/worker.go`: `Run(ctx context.Context, deps Dependencies) error`;
  `Dependencies{Config, DB, Objects}`. Обработчики заданий находятся в отдельных файлах.
- `api/openapi.yaml`: точные JSON-контракты, ошибки, Bearer security и курсоры.
- `admin/src/api.ts`: общий авторизованный HTTP-клиент; `admin/src/auth.tsx`: состояние сессии.
- `tests/integration/`: сценарии с реальными PostgreSQL/S3/API; `tests/browser/`: Playwright.
- `Dockerfile`, `compose.yaml`, `.env.example`, `Makefile`, `README.md`: сборка и эксплуатация.

## Task 1: Запуск, конфигурация, схема данных и среда тестов

**Files:** `go.mod`, `cmd/familychat/main.go`, `internal/config/*`, `internal/database/*`,
`internal/httpapi/server.go`, `internal/httpapi/health.go`, `compose.yaml`, `.gitignore`, `api/openapi.yaml`.

**Interfaces:** создаёт `Config`, `database.Open`, `database.Migrate`, `httpapi.New` и health endpoints.

- [x] Создать Git-репозиторий и рабочую ветку; закрепить исходную спецификацию и план коммитом.
- [ ] Написать `TestConfigRejectsMissingSecrets`, `TestMigrationIsRepeatable`,
  `TestReadinessUnavailable`: невалидная конфигурация отклоняется, миграция повторяема,
  недоступные PostgreSQL/S3 возвращают readiness `503`, liveness остаётся `200`.
- [ ] Запустить тесты и зафиксировать ожидаемые отказы до реализации.
- [ ] Реализовать команды и конфигурацию; добавить миграции всех таблиц спецификации,
  внешние ключи, уникальные пары личных чатов, счётчики чатов, индексы истории и поиска.
  `bootstrap-admin` читает пароль из TTY или файла секрета, не из аргумента командной строки.
- [ ] Поднять изолированные PostgreSQL и S3 для тестов; проверить реальную миграцию,
  отмену по context, graceful shutdown и отсутствие секретов в ошибках запуска.
- [ ] Выполнить `go test ./internal/config ./internal/database ./internal/httpapi` и коммит.

## Task 2: Вход, сессии, регистрация и административный API

**Files:** `internal/auth/{auth,password,session,ratelimit}.go`,
`internal/httpapi/{auth,admin,users}.go`, соответствующие `_test.go`, `api/openapi.yaml`.

**Interfaces:** `auth.Service`; маршруты `/auth/register`, `/auth/login`, `/auth/logout`,
`/auth/me`, `/auth/change-password`, `/admin/users`, `/admin/users/{id}`,
`/admin/settings/registration`, `/users` под `/api/v1`.

- [ ] Написать тесты закрытой регистрации, одинакового отказа при неверном логине/пароле,
  нечувствительности логина к регистру, хеширования пароля, истечения и отзыва сессии.
  В `TestLastAdminCannotBeBlockedConcurrently` параллельные блокировки сохраняют администратора.
- [ ] Запустить тесты: ожидаются отказы отсутствующей функциональности.
- [ ] Реализовать Argon2id, серверные сессии, throttling и bootstrap первого администратора.
  Настройка регистрации и выдача роли проверяются транзакционно;
  обычная регистрация никогда не принимает роль из тела запроса.
- [ ] Реализовать административные операции, пагинацию списка и поиск пользователей;
  проверять авторизацию также при прямых HTTP-запросах вне панели.
- [ ] Проверить `401`, `403`, запрет передачи токена в URL/cookie вместо заголовка,
  отзыв сессий при блокировке и смене пароля; `go test -race ./internal/auth ./internal/httpapi`.
- [ ] Зафиксировать коммит с работающим auth/admin API и OpenAPI-контрактом.

## Task 3: Личные и групповые чаты

**Files:** `internal/httpapi/chats.go`, `internal/httpapi/chats_test.go`, `api/openapi.yaml`.

**Interfaces:** `/chats`, `/chats/direct`, `/chats/{id}`, `/chats/{id}/members`,
`/chats/{id}/members/{userID}`, `/chats/{id}/leave`, `/chats/{id}/owner`.
Владелец может удалить группу через `DELETE /chats/{id}`; удаление логическое,
с атомарным отзывом доступа всех участников и постановкой вложений в очередь очистки.

- [ ] Написать `TestConcurrentDirectChatCreation`, `TestGroupRoles`,
  `TestRemovedMemberCannotReadChat`, `TestOwnerMustTransferBeforeLeaving`,
  `TestDeletedGroupRevokesAccessAndSchedulesCleanup`.
- [ ] Убедиться в RED; затем реализовать членство и роли в транзакциях.
  Блокировки членства сериализуют исключение с действиями в чате.
- [ ] Проверить, что перестановка двух пользователей не создаёт другой личный чат;
  новый участник получает историю, исключённый не читает её, администратор сервера
  без членства тоже не читает. Личный чат нельзя превратить в группу через API состава.
- [ ] Выполнить интеграционные тесты чатов; обновить OpenAPI и сделать коммит.

## Task 4: Сообщения, история, поиск и прочтение

**Files:** `internal/httpapi/{messages,search,readstate}.go`, их тесты, `api/openapi.yaml`.

**Interfaces:** `/chats/{id}/messages`, `/chats/{id}/search`, `/chats/{id}/read`;
вход сообщения `{client_message_id, text, attachment_ids}`, выход `{id, seq, ...}`.

- [ ] Написать тесты идемпотентной параллельной отправки, курсоров до/после номера,
  монотонного прочтения, русского поиска и запрета чужого/неготового вложения.
  Повтор `client_message_id` с другим содержимым даёт `409`, а не молчаливую подмену.
- [ ] Убедиться в RED; реализовать сообщение, его номер и событие одной транзакцией,
  уникальность `(chat_id, sender_id, client_message_id)` и курсоры без OFFSET.
- [ ] Добавить поиск текста/подписей и имён файлов с проверкой текущего членства,
  ограничением длины запроса и стабильным порядком выдачи.
- [ ] Проверить восстановление истории после рестарта и конкурирующую отправку;
  выполнить интеграционные тесты и сделать коммит.

## Task 5: WebSocket и доставка событий

**Files:** `internal/events/*`, `internal/httpapi/websocket.go`, их тесты, `api/openapi.yaml`.

**Interfaces:** `POST /auth/websocket-ticket`, `GET /ws`; подключение использует одноразовый
билет, основной Bearer-токен не попадает в URL. `Hub` использует события PostgreSQL.

- [ ] Написать тесты однократного потребления билета, истечения, Origin, отзыва сессии,
  исключения участника между фиксацией события и доставкой, медленного подписчика.
- [ ] Убедиться в RED; реализовать атомарное потребление билета и Hub с ограниченными
  очередями, ping/pong, размерами кадров, отменой соединений и проверкой сессий.
  Билет не журналируется; в production соединение использует WSS.
- [ ] Доставлять события только действующим участникам. Разрыв или повтор доставки
  компенсируется REST-историей; исключение/отзыв немедленно закрывают соответствующий доступ.
- [ ] Проверить два реальных WebSocket-клиента и повторное подключение;
  `go test -race ./internal/events ./internal/httpapi`, затем коммит.

## Task 6: S3-загрузка и авторизованная выдача файлов

**Files:** `internal/objects/*`, `internal/httpapi/files.go`, их тесты, `api/openapi.yaml`.

**Interfaces:** `objects.Store`; `/chats/{id}/files`, `/files/{id}`,
`/files/{id}/content`, `/files/{id}/preview`; последние два поддерживают GET/HEAD.

- [ ] Написать матрицу `TestFileAuthorization`: Bearer владельца/участника, отсутствие
  заголовка, URL/cookie-only, чужой пользователь, отозванная сессия, исключённый участник.
  Повторить её для оригинала, превью, HEAD, условного запроса и Range.
- [ ] Написать `TestUploadAbortReleasesQuota`, `TestConcurrentUploadsRespectQuota`,
  `TestRangeAfterMembershipRevoked`, `TestFileResponsesNeverRedirect` и наблюдать RED.
- [ ] Реализовать резервирование квоты и потоковую загрузку без полного буфера файла;
  уникальные ключи, отмену multipart при ошибке, фиксацию после S3 и задания очистки.
  Незавершённый объект нельзя привязать к сообщению; неподтверждённый размер не обходит лимит.
- [ ] Реализовать проверку доступа до S3/Range/условных заголовков;
  корректные `200/206/401/404/416`, `Content-Length`, `Content-Range`, HEAD без тела,
  `private, no-store`, безопасное имя скачивания и `nosniff`.
- [ ] Проверить реальный S3 round-trip и контроль памяти на большом файле;
  выполнить интеграционные тесты файлов, обновить OpenAPI и сделать коммит.

## Task 7: Превью и уборка незавершённых операций

**Files:** `internal/worker/{worker,preview,cleanup}.go`, тесты, FFmpeg-слой `Dockerfile`.

**Interfaces:** `worker.Run`; задания PostgreSQL с lease и состояниями спецификации.

- [ ] Создать маленькие реальные JPEG/PNG/MP4/WebM фикстуры и повреждённые файлы;
  проверить `ready`, `failed`, `unsupported`, повтор после истечения lease и отмену задачи.
- [ ] Наблюдать RED; реализовать выбор заданий с `FOR UPDATE SKIP LOCKED`, lease,
  ограниченные повторы, вызов FFmpeg без shell и запись превью в S3.
- [ ] Ограничить время/память/параллелизм, размеры кадра и временные файлы;
  исходник сохраняется при ошибке превью. По готовности записывать событие в PostgreSQL.
- [ ] Реализовать идемпотентную очистку истёкших резервов и объектов;
  активное продлеваемое задание/загрузка не удаляется уборщиком.
- [ ] Проверить убийство и перезапуск worker между записью S3 и фиксацией БД;
  выполнить тесты с настоящим FFmpeg и S3, затем коммит.

## Task 8: Сигнализация звонков и TURN

**Files:** `internal/httpapi/calls.go`, `internal/events/signaling.go`,
`internal/calls/*`, тесты, `deploy/coturn/*`, `api/openapi.yaml`.

**Interfaces:** `/chats/{id}/calls`, `/calls/{id}`, `/calls/{id}/{accept,reject,cancel,end,ice}`;
WebSocket-команды `{type, call_id, data}` для SDP/ICE и восстановления выбранного устройства.

- [ ] Написать тесты переходов состояния, конкурентного busy, принятия на двух устройствах,
  `45s` таймаута, `30s` переподключения, чужого SDP/ICE и завершения после рестарта.
  В тестах время управляемое: не ждать реальные десятки секунд для каждой проверки.
- [ ] Наблюдать RED; реализовать атомарные переходы и занятость обоих участников,
  привязку выбранного устройства, лимиты сигнализации и очистку зависших состояний.
- [ ] Реализовать временные TURN-реквизиты и обновление только для активного звонка;
  настроить coturn с квотами и ограничением peer-адресов.
- [ ] Проверить реальную TURN allocation с временными реквизитами и отказ с неверными;
  выполнить тесты звонков, сохранить контракт и коммит.

## Task 9: React-админка

**Files:** `admin/package.json`, `admin/package-lock.json`, `admin/src/{api,auth,App}.tsx/ts`,
`admin/src/pages/{Login,Users,Settings}.tsx`, стили, Vitest и Playwright-тесты.

**Interfaces:** API задач 2 и 1; статика по `/admin/` в образе Go-сервера.

- [ ] Написать тесты входа, запрета обычному пользователю, управления регистрацией,
  создания/блокировки аккаунта, обработки ошибки сохранения и позднего ответа после logout.
- [ ] Убедиться в RED; реализовать React + TypeScript SPA с понятными русскими подписями,
  состояниями loading/empty/error, поиском и пагинацией пользователей.
- [ ] Общий API-клиент отправляет Bearer только своему origin; токен хранится в памяти.
  Отзыв/выход отменяет запросы и не позволяет поздним ответам обновлять предыдущую сессию.
- [ ] Добавить отдельную frontend-стадию Docker; маршруты `/api/*` и `/files/*`
  не перехватываются SPA fallback. Node.js не требуется в runtime-образе.
- [ ] Выполнить typecheck, lint, Vitest, production build; Playwright проверяет реальную
  страницу `/admin/` и реальные изменения в API. Сделать коммит.

## Task 10: Сквозные проверки, упаковка и инструкции запуска

**Files:** `tests/integration/*`, `tests/browser/*`, `tests/browser/fixtures/call-client.html`,
`Dockerfile`, `compose.yaml`, `.env.example`, `Makefile`, `README.md`, `api/openapi.yaml`.

**Interfaces:** весь публичный контракт; тестовая HTML-страница не является клиентом продукта.

- [ ] Написать E2E-сценарий bootstrap → регистрация → личный/групповой чат → файл →
  превью → поиск → исключение → отказ доступа → перезапуск → сохранённая история.
- [ ] Написать тест двух браузерных WebRTC-клиентов с искусственными камерами/микрофонами:
  дождаться удалённых треков, роста `inbound-rtp.bytesReceived` и кадров видео.
  Повторить с `iceTransportPolicy: relay`; проверить выбранную ICE-пару через TURN.
- [ ] Запустить сценарии; исправления каждого обнаруженного дефекта подтверждать
  воспроизводящим тестом. Не подменять успешное медиа одним состоянием `accepted`.
- [ ] Завершить Compose: API, worker, PostgreSQL, coturn, локальный S3-профиль,
  healthchecks, постоянные тома, миграции, non-root и documented ports/NAT/TLS.
- [ ] В README добавить точные команды запуска, bootstrap, восстановления PostgreSQL+S3,
  пример Bearer-загрузки файла и Range, клиентский контракт WebSocket и звонков,
  описание границы поддержки фоновых звонков и закрытой регистрации по умолчанию.
- [ ] Проверить OpenAPI по фактическим маршрутам; выполнить `gofmt`, `go vet ./...`,
  `go test -race ./...`, `go build ./...`, frontend-проверки, `docker compose config`,
  сборку образов и E2E. Проверку между реальными внешними сетями отмечать отдельно.
- [ ] Выполнить итоговое ревью кода, исправить существенные замечания с тестами,
  зафиксировать коммиты и перечислить выполненные и недоступные live-проверки пользователю.

## Порядок выполнения

Задачи 1–8 выполняются последовательно: следующий модуль опирается на проверенные
транзакции, права и контракты предыдущего. Задача 9 использует готовый admin API;
задача 10 завершает интеграцию и проверяет всё приложение.

Рекомендуемый способ — выполнение в текущей задаче одним исполнителем с итоговым
независимым ревью. Это уменьшает риск расхождения тесно связанных API и схемы данных.
Делегирование отдельных задач возможно после выбора пользователем такого способа работы.
