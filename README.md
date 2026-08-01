# sonarr-torrent-importer

Консервативный импортёр завершённых qBittorrent-раздач в Sonarr с явным сопоставлением файлов эпизодам. `v0.1.0` — ручной Phase 0 workflow для строгих имён `[NN].mkv` в подтверждённом Sonarr season context.

Импортёр не переименовывает, не перемещает и не удаляет torrent-файлы. qBittorrent используется только для чтения manifest; все пути и import decisions повторно подтверждает Sonarr.

## Границы Phase 0

- Sonarr v4.
- qBittorrent Web API 2.8.2 или новее.
- Один явно выбранный `downloadId`; `queueId` можно использовать в dry run, чтобы разрешить соответствующий `downloadId`.
- Только точный шаблон `[01].mkv` … `[99].mkv`, включая файлы внутри torrent folder.
- Dry run по умолчанию; execute требует точного подтверждения `downloadId` и неизменившийся `planToken` из dry run.
- До импорта повторно проверяются queue context, manual-import payload, episode metadata и hash manifest.
- Успех подтверждается одновременно новой Sonarr history, episode state и episode-file metadata.
- Queue финализируется через Sonarr без удаления torrent; после этого manifest и активный seeding проверяются ещё раз.

Write-ahead intent и последний operation result атомарно хранятся под `/data`; межпроцессный lock не допускает параллельные execute. После неопределённого ответа mutation не повторяется вслепую: следующий execute с теми же `downloadId` и `planToken` продолжает проверку postconditions. SQLite audit history, стандартные `SxxEyy`, absolute/date patterns, polling и review API относятся к следующим этапам.

## Запуск

Скачайте `compose.example.yaml` и `.env.example` из GitHub Release `v0.1.0`, затем заполните реальные endpoints и credentials:

```bash
cp .env.example .env
docker compose -f compose.example.yaml pull
docker compose -f compose.example.yaml up -d
```

Compose использует versioned image `ghcr.io/zenderg/sonarr-torrent-importer:v0.1.0`, запускает его без root и сохраняет safety journal в named volume. `/data` должен быть обычным локальным Docker volume или локальным bind mount с рабочими `flock`, atomic rename и `fsync`; NFS/CIFS для `v0.1.0` не поддерживаются.

HTTP API слушает только localhost в Compose-примере. У API нет собственной аутентификации, поэтому публиковать порт в недоверенную сеть нельзя.

Проверка состояния:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/api/v1/status
```

Dry run:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/imports/dry-run \
  -H 'Content-Type: application/json' \
  -d '{"downloadId":"<torrent-info-hash>"}'
```

Dry run с `queueId` вернёт разрешённый `selection.downloadId`, `context.downloadId` и `planToken`. Execute всегда использует этот `downloadId`, повторяет его в `confirmDownloadId` и передаёт неизменённый token:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/imports/execute \
  -H 'Content-Type: application/json' \
  -d '{"downloadId":"<torrent-info-hash>","confirmDownloadId":"<torrent-info-hash>","planToken":"sha256:<dry-run-token>"}'
```

Одноразовый CLI использует ту же конфигурацию и те же safety checks:

```bash
docker compose -f compose.example.yaml run --rm importer \
  run --download-id '<torrent-info-hash>'

docker compose -f compose.example.yaml run --rm importer \
  run --download-id '<torrent-info-hash>' \
  --execute --confirm-download-id '<torrent-info-hash>' \
  --plan-token 'sha256:<dry-run-token>'
```

Не запускайте execute, пока dry run не вернул `outcome: "ready"`, `canExecute: true` и ожидаемые `episodeIds` для каждого media file. Если plan изменился, execute завершится с `DRY_RUN_PLAN_CHANGED` без mutation — выполните новый dry run и снова проверьте результат.

## Сборка из checkout

Путь для разработки использует отдельный Compose override:

```bash
cp .env.example .env
docker compose -f compose.example.yaml -f compose.dev.yaml up -d --build
```

Production deployment не должен подключать `compose.dev.yaml`.

## Документация

- [Контекст и исходная концепция](docs/project-context.md)
- [Ревью концепции и рекомендуемый MVP](docs/concept-review.md)
- [Контракт релизов и Docker-образа](docs/releases.md)
- [Разработка и интеграционные границы](docs/development.md)
