# sonarr-torrent-importer

Надёжный импорт завершённых qBittorrent-раздач, в которых файлы названы как `[01].mkv`, `[02].mkv` и поэтому не распознаются Sonarr автоматически.

Импортёр получает точный series/season context из очереди Sonarr, сопоставляет каждый `[NN].mkv` одному episode ID, строит понятное Sonarr имя вроде `Futurama.S01E01.WEBDL-720p.mkv` и переименовывает файл через Web API qBittorrent. После этого Sonarr штатно импортирует файл; успех подтверждается одновременно по history, episode state и episode-file metadata. Раздача остаётся в qBittorrent и продолжает сидироваться.

## Поддерживаемый сценарий

- Sonarr v4.
- qBittorrent v5 и Web API 2.11.0 или новее.
- Явно выбранный torrent `downloadId`; `queueId` можно использовать в dry run для разрешения соответствующего hash.
- Multi-file torrent с файлами `[01].mkv` … `[99].mkv` внутри стабильного torrent root.
- Один точный episode match на каждый выбранный завершённый media file.
- Активное состояние seeding: `uploading`, `stalledUP` или `forcedUP`.

Single-file torrents, folder rename, case-only rename, неоднозначные episode mappings и уже занятые canonical paths блокируются до любых изменений. Импортёр не удаляет torrent и не меняет содержимое файла; меняется только имя torrent file через qBittorrent.

## Гарантии выполнения

Dry run ничего не изменяет и возвращает исходный и canonical path каждого файла. Execute требует точного `downloadId`, повторного `confirmDownloadId` и неизменившегося `planToken`.

Перед каждой мутацией сохраняется write-ahead operation record под `/data`. HTTP 200 от qBittorrent не считается доказательством: importer опрашивает manifest и подтверждает новый path по стабильному file index, size, progress и priority. После рестарта незавершённая операция продолжается по тому же token без повторения уже доказанного rename или Sonarr command.

После canonical rename importer ждёт штатный completed-download import Sonarr. Если Sonarr не завершил его за `COMMAND_TIMEOUT`, importer заново получает canonical manual-import candidates и использует проверенный explicit mapping. В обоих случаях финальный успех требует нового `downloadFolderImported` history record, ожидаемого episode file, пустой queue и сохранённой seeding-раздачи с canonical manifest.

## Запуск

Скачайте `compose.example.yaml` и `.env.example` из GitHub Release `v1.0.0`, затем заполните endpoints и credentials:

```bash
cp .env.example .env
docker compose -f compose.example.yaml pull
docker compose -f compose.example.yaml up -d
```

Compose использует образ `ghcr.io/zenderg/sonarr-torrent-importer:v1.0.0`, запускает контейнер без root и сохраняет operation journal в named volume. `/data` должен находиться на локальном Docker volume или bind mount с корректными `flock`, atomic rename и `fsync`; NFS/CIFS не поддерживаются.

HTTP API в примере слушает только localhost. У сервиса нет собственной аутентификации, поэтому его порт нельзя публиковать в недоверенную сеть.

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

Execute использует `selection.downloadId` и `planToken` из проверенного dry run:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/imports/execute \
  -H 'Content-Type: application/json' \
  -d '{"downloadId":"<torrent-info-hash>","confirmDownloadId":"<torrent-info-hash>","planToken":"sha256:<dry-run-token>"}'
```

Не запускайте execute, пока dry run не вернул `outcome: "ready"`, `canExecute: true`, ожидаемые `episodeIds` и правильные `rename.fromPath`/`rename.toPath` для каждого media file.

Одноразовый CLI использует тот же workflow:

```bash
docker compose -f compose.example.yaml run --rm importer \
  run --download-id '<torrent-info-hash>'

docker compose -f compose.example.yaml run --rm importer \
  run --download-id '<torrent-info-hash>' \
  --execute --confirm-download-id '<torrent-info-hash>' \
  --plan-token 'sha256:<dry-run-token>'
```

## Разработка и проверка

```bash
cp integration.env.example .env
./scripts/run_integration_e2e.sh
```

E2E script поднимает отдельный Compose stack с закреплёнными tag и digest Sonarr/qBittorrent, создаёт реальный 301-секундный media fixture и отправляет torrent через Sonarr release push. Во время qBittorrent rename он принудительно перезапускает importer, затем доказывает восстановление по тому же token без повторного rename, Sonarr auto-import, history/episode-file, post-import category, queue finalization и active seeding. Добавление Futurama использует штатный внешний metadata lookup Sonarr, поэтому для E2E нужен доступ в интернет.

Подробности:

- [Разработка и локальный integration stack](docs/development.md)
- [Контракт релизов и Docker-образа](docs/releases.md)
- [Исходный продуктовый контекст](docs/project-context.md)
- [Историческое ревью концепции](docs/concept-review.md)
