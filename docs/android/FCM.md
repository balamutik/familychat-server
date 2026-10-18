# Android push через FCM

Расширение добавляет FCM HTTP v1, сохраняя APNs. Серверный service account JSON хранится вне Git и APK. Firebase project должен совпадать с project_id ключа; application ID — с Android-приложением.

## Настройка

1. В проекте Firebase включить Cloud Messaging API v1 и выдать сервисному аккаунту разрешение отправки сообщений. Сохранить его JSON в защищённом файле на сервере.
2. Задать `FCM_CREDENTIALS_FILE` (путь на хосте), `FCM_PROJECT_ID`, `FCM_APPLICATION_ID` (для этой сборки `com.familychat.android`). Все три поля обязательны вместе; process env внутри контейнеров получает путь `/run/secrets/fcm_credentials`.
3. Подключить `compose.fcm.yaml` к существующим Compose-файлам. Secret монтируется read-only для UID/GID10001. В обычном Docker Compose file-backed secrets могут сохранять UID файла хоста: заранее обеспечить чтение JSON процессом UID10001 (владельческие права0400); не делать ключ доступным всем пользователям. Не передавать содержимое JSON в командной строке, переменных или логах.
4. Применить штатные миграции сервера до запуска новой API/worker версии. Миграция `0016_android_push.sql` сохраняет старые регистрации и добавляет платформу/FCM token и типы задач. Обновлять API и worker совместно; старый worker не умеет маршрутизировать FCM.
5. Для Android добавить владельческий `google-services.json` в `android/app/` и пересобрать. Файл не коммитится. Без конфигурации foreground-переписка работает, в настройках указан статус недоступности push.

Пример локальной проверки конфигурации (используйте ваш действующий HTTPS override):

```sh
docker compose -f compose.yaml -f compose.fcm.yaml config --quiet
```

## Контракт

`PUT /api/v1/push/devices/{deviceUUID}`:

```json
{"client_server_id":"local-server-profile-uuid","platform":"android","fcm_token":"opaque-case-sensitive-token"}
```

Token не hex, регистр сохраняется, длина 1–4096 байт, без whitespace/control characters. APNs payload с `alert_token`/`voip_token` и отсутствующим platform сохраняется. Смешанные формы отклоняются. Изменение session/token/platform/client_server_id удаляет старые queued jobs; идентичный PUT сохраняет очередь. Удаление регистрации/сессии каскадно удаляет её jobs.

FCM data включает только event_type (`message`, `call`, `call_state`), локальный server_id/client_server_id, chat_id/message_id/call_id, имя отправителя, вид/состояние звонка и expires_at. Текст, ключи и credentials не передаются. Android подтверждает доступ и актуальное состояние REST-запросом перед уведомлением.

## Доставка и диагностика

- HTTP v1 использует cached OAuth access token с обновлением до expiry; ошибки не содержат token/private key/server response.
- Срочный user-visible push имеет HIGH priority. TTL сообщения — 24 ч, изменения звонка — 30 сек; входящий звонок — оставшееся время до created_at +45 сек. Просроченный звонок не отправляется.
- Worker проверяет сессию, существование сообщения, hide/delete, membership, disabled и блокировки; звонок должен ещё быть ringing. До пяти попыток; задержка 30×attempt сек, срок звонка проверяется заново.
- `UNREGISTERED` удаляет только текущую устаревшую FCM-регистрацию; отзыв старого token после ротации не удаляет новый. Другие HTTP ошибки проходят ограниченный retry. OAuth401 сбрасывает cached access token.
- Рабочая диагностика: отсутствие ошибок конфигурации при запуске, счётчики/типы jobs и обезличенная строка `push delivery failed (kind, attempt)`. Не публикуйте payload/credentials из БД.
- После accept/cancel/reject/end создаётся call_state job для Android-устройств участников. Уведомление о ringing само истекает; REST остаётся авторитетом. Android повторно проверяет call state перед accept.

## Приёмка

Unit tests проверяют OAuth, payload allowlist, access-token cache и TTL с fake HTTP provider. Integration tests проверяют APNs backward compatibility, Android token case, jobs, повторную регистрацию и ротацию в отдельной PostgreSQL. Реальную доставку необходимо подтвердить с владельческим Firebase-проектом, физическим устройством, locked/background/restart, вторым устройством и выданными permissions. Без этого AC-14 не считается пройденным.

FCM требует Google Play services. No-GMS/Huawei-поддержка не входит в базовую поставку. Force-stop, запрет уведомлений и ограничения производителя могут предотвращать доставку; синхронизация после открытия восстанавливает актуальные данные.

## Совместимое Unicode имя файла

Android передаёт оригинальное percent-encoded имя одновременно в стандартном `X-File-Name*` и совместимом `X-File-Name-Encoded` с тем же RFC5987 значением. API понимает оба поля; конфликт значений отклоняется. Наличие обычного `X-File-Name` сохраняет совместимость со старыми серверами. Дополнительный заголовок проходит через прокси с обычными правилами проверки имён HTTP-полей. Настройки безопасности HTTPS-прокси не меняются. Для точного сохранения Unicode-имени через прокси, отбрасывающий `X-File-Name*`, требуется обновление API с этой поддержкой.
