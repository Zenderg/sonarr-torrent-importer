# sonarr-torrent-importer

Надёжный импорт завершённых qBittorrent-раздач, в которых файлы названы как `[01].mkv`, `[02].mkv` и поэтому не распознаются Sonarr автоматически.

Импортёр получает точный series/season context из очереди Sonarr, сопоставляет каждый `[NN].mkv` одному episode ID, строит понятное Sonarr имя вроде `Futurama.S01E01.WEBDL-720p.mkv` и переименовывает файл через Web API qBittorrent. После этого Sonarr штатно импортирует файл; успех подтверждается одновременно по history, episode state и episode-file metadata. Раздача остаётся в qBittorrent и продолжает сидироваться.

Обновляемые раздачи тоже поддерживаются. После первого обычного импорта release можно явно зарегистрировать как rolling: importer опрашивает точный Prowlarr result, сохраняет неизменяемые `.torrent`-байты, поднимает следующую revision в отдельном staging-root, переиспользует старые данные безопасным копированием и recheck, докачивает только недостающее и импортирует в Sonarr только новые серии.

## Поддерживаемый сценарий

- Sonarr v4.
- qBittorrent v5 и Web API 2.11.0 или новее.
- Явно выбранный torrent `downloadId`; `queueId` можно использовать в dry run для разрешения соответствующего hash.
- Multi-file torrent с файлами `[01].mkv` … `[99].mkv` внутри стабильного torrent root.
- Один точный episode match на каждый выбранный завершённый media file.
- Активное состояние seeding: `uploading`, `stalledUP` или `forcedUP`.

Для rolling releases дополнительно требуются Prowlarr, qBittorrent Web API 2.14.0 или новее, сборка qBittorrent с libtorrent 2.x и общий media mount в контейнер importer. Поддерживаются v1, hybrid и pure v2 torrents. Новая revision должна сохранять все ранее импортированные episodes и добавлять хотя бы один отсутствующий. Удаление, изменение размера или содержимого старой серии, чужой существующий Sonarr episode-file, unsafe path и неоднозначный Prowlarr selector блокируют замену.

Single-file torrents, folder rename, case-only rename, неоднозначные episode mappings и уже занятые canonical paths блокируются до любых изменений. Импортёр не удаляет torrent и не меняет содержимое файла; меняется только имя torrent file через qBittorrent.

## Гарантии выполнения

Dry run ничего не изменяет и возвращает исходный и canonical path каждого файла. Execute требует точного `downloadId`, повторного `confirmDownloadId` и неизменившегося `planToken`.

Перед каждой мутацией сохраняется write-ahead operation record под `/data`. HTTP 200 от qBittorrent не считается доказательством: importer опрашивает manifest и подтверждает новый path по стабильному file index, size, progress и priority. После рестарта незавершённая операция продолжается по тому же token без повторения уже доказанного rename или Sonarr command.

После canonical rename importer ждёт штатный completed-download import Sonarr. Если Sonarr не завершил его за `COMMAND_TIMEOUT`, importer заново получает canonical manual-import candidates и использует проверенный explicit mapping. В обоих случаях финальный успех требует нового `downloadFolderImported` history record, ожидаемого episode file, пустой queue и сохранённой seeding-раздачи с canonical manifest.

Rolling workflow имеет отдельный durable release journal `/data/rolling`, но использует тот же глобальный execution lock. Перед внешними командами сохраняется intent, а после рестарта проверяется фактический qBittorrent/Sonarr postcondition. Кандидат переводится в force-start, чтобы download/upload queue не могла заблокировать замену, и остаётся force-started после завершения. Новые episodes импортируются принудительно в режиме `copy`. Затем новая раздача останавливается, проходит полный qBittorrent recheck, каждый source-файл сверяется по SHA-256 и раздача снова начинает сидироваться. Только после этого удаляется старая запись qBittorrent с жёстко заданным `deleteFiles=false`; сохранность старых файлов повторно проверяется после удаления записи.

Старые файлы автоматически не удаляются: это намеренная защита от потери данных. Новая revision живёт в `.sonarr-torrent-importer/<releaseId>/<infoHash>` внутри qBittorrent media root. Копии независимы — hardlink для reuse не создаётся, поэтому qBittorrent не может повредить уже импортированный Sonarr файл.

## Запуск

Скачайте `compose.example.yaml` и `.env.example` из GitHub Release `v1.1.0`, затем заполните endpoints и credentials:

```bash
cp .env.example .env
docker compose -f compose.example.yaml pull
docker compose -f compose.example.yaml up -d
```

Compose использует образ `ghcr.io/zenderg/sonarr-torrent-importer:v1.1.0`, запускает контейнер без root и сохраняет operation journal в named volume. `/data` должен находиться на локальном Docker volume или bind mount с корректными `flock`, atomic rename и `fsync`; NFS/CIFS не поддерживаются.

HTTP API в примере слушает только localhost. У сервиса нет собственной аутентификации, поэтому его порт нельзя публиковать в недоверенную сеть.

### Обновляемые раздачи

Раскомментируйте rolling-блок в `.env`. `QBITTORRENT_MEDIA_HOST_PATH` должен указывать на тот же host directory/volume, который qBittorrent видит как `QBITTORRENT_MEDIA_ROOT`; importer видит его как `IMPORTER_MEDIA_ROOT`. `SONARR_MEDIA_ROOT` — путь к этому же storage в namespace контейнера Sonarr (он может отличаться от qBittorrent). `QBITTORRENT_MEDIA_GID` — группа с read/write доступом к storage; она добавляется контейнеру importer как supplemental group. Каталог должен быть group-writable и желательно иметь setgid bit. Запускайте с overlay:

```bash
docker compose -f compose.example.yaml -f compose.rolling.example.yaml up -d
```

В Prowlarr у выбранного indexer отключите `Redirect`: importer намеренно запрещает cross-origin redirect и не передаёт tracker credentials за пределы Prowlarr. Enrollment разрешён только от уже завершённой обычной операции этого importer:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/rolling-releases \
  -H 'Content-Type: application/json' \
  -d '{
    "releaseId":"futurama-s01",
    "downloadId":"<current-qbittorrent-hash>",
    "confirmDownloadId":"<current-qbittorrent-hash>",
    "indexerId":7,
    "guid":"<exact-stable-prowlarr-guid>",
    "query":"Futurama S01"
  }'
```

`downloadId` — поле `hash` из qBittorrent Web API: v1 SHA-1 для v1-only либо первые 20 байт v2 SHA-256 для v2/hybrid. Полные v1/v2 info-hash сохраняются и сверяются отдельно. `indexerId + guid` — стабильная identity одной обновляемой публикации; title и новый info-hash могут меняться. Source URL и Prowlarr API key в durable state не сохраняются. После enrollment проверка выполняется каждые `REVISION_POLL_INTERVAL`. Немедленная проверка и состояние:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/rolling-releases/check \
  -H 'Content-Type: application/json' \
  -d '{"releaseId":"futurama-s01"}'

curl http://127.0.0.1:8080/api/v1/rolling-releases/futurama-s01
```

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
./scripts/run_rolling_integration_e2e.sh
```

E2E script поднимает отдельный Compose stack с закреплёнными tag и digest Sonarr/qBittorrent, создаёт реальный 301-секундный media fixture и отправляет torrent через Sonarr release push. Во время qBittorrent rename он принудительно перезапускает importer, затем доказывает восстановление по тому же token без повторного rename, Sonarr auto-import, history/episode-file, post-import category, queue finalization и active seeding. Добавление Futurama использует штатный внешний metadata lookup Sonarr, поэтому для E2E нужен доступ в интернет.

Rolling E2E продолжает этот контракт двумя реальными torrent revisions `[01]` → `[01], [02]`: принудительно перезапускает importer после применённого add-intent и после применённого keep-content delete-intent, доказывает reuse первой серии, webseed-докачку второй, `copy`-импорт только E02, повторный полный recheck, точную сохранность старых байтов и active seeding новой revision.

Подробности:

- [Разработка и локальный integration stack](docs/development.md)
- [Контракт релизов и Docker-образа](docs/releases.md)
- [Исходный продуктовый контекст](docs/project-context.md)
- [Историческое ревью концепции](docs/concept-review.md)
