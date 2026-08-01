# Concept Review

Дата ревью: 2026-08-01

Этот документ — источник истины для проверенных ограничений и рекомендуемых границ MVP на стадии проектирования. Он оценивает исходный [`project-context.md`](project-context.md), но не заменяет описание продуктовой мотивации и не является документацией уже реализованной системы. Релизный контракт описан отдельно в [`releases.md`](releases.md).

## Краткий вердикт

Проблема реальна, а полезное ядро проекта существует. Автоматический импорт плохо названных файлов с явным сопоставлением эпизодов технически реализуем. Штатное удаление обработанного torrent из Sonarr Activity без удаления данных также реализуемо.

Однако полный проект в текущем виде начинать рано:

- обязательное переименование исходных torrent-файлов не требуется и создаёт дополнительные риски;
- критерий «каждый видеофайл должен быть импортирован» не покрывает дубликаты, samples, extras и эпизоды, уже имеющиеся в лучшем качестве;
- повторное использование данных между ревизиями torrent нельзя гарантировать;
- обнаружение новой ревизии rolling torrent и стабильная идентификация логического релиза пока не спроектированы;
- импорт обычных завершённых загрузок и управление rolling torrents — две разные задачи с разными рисками, их не следует связывать в одном MVP.

Рекомендация: сделать небольшой Phase 0 для одной реальной проблемной раздачи. Если он стабильно работает на нескольких настоящих примерах, развивать его в маленький сервис. Rolling torrents исследовать отдельно и не обещать как гарантированную возможность.

## Статус выводов

На дату исходного ревью в репозитории не было реализации. Сейчас Phase 0 integration spike реализован для Sonarr v4 и qBittorrent Web API 2.8.2+, но основной API workflow ещё нужно доказать на реальной раздаче. Поддерживаемая реализацией граница и команды проверки описаны в [`development.md`](development.md).

Это важно: документ следует воспринимать как RFC, а не как подтверждение того, что весь workflow работает на реальных версиях Sonarr, qBittorrent и конкретного indexer/tracker.

## Оценка заявленных проблем

### 1. Плохо названные файлы требуют ручного импорта

Проблема реальна и в детерминированных случаях решаема.

Sonarr поддерживает ручной импорт, в котором для каждого файла передаются явные `SeriesId` и `EpisodeIds`. Следовательно, файл вроде `[03].mkv` необязательно переименовывать в `Show.S02E03.mkv` до импорта. Внешний сервис может определить нужный episode ID и передать его Sonarr.

Это решает повторяющиеся случаи, когда одновременно известны:

- torrent/download ID;
- series ID;
- season number или другой однозначный контекст;
- локальный, абсолютный или датированный номер эпизода;
- ровно один допустимый эпизод Sonarr.

Это не решает и не должно автоматически решать случаи, когда несколько эпизодов одинаково правдоподобны. Отказ от угадывания в исходном документе корректен.

### 2. Season pack остаётся висеть в Sonarr Activity

Проблема реальна. В Sonarr уже существует штатная операция, которая прекращает tracking download, не удаляет torrent и просит download client назначить post-import category.

Практически сервис должен вызывать Sonarr:

```text
DELETE /api/v3/queue/{queueId}?removeFromClient=false&changeCategory=true
```

Для qBittorrent Sonarr сам вызывает `MarkItemAsImported`, меняет категорию на настроенную imported category и прекращает tracking download. Это лучше прямого изменения категории через qBittorrent API, потому что Sonarr одновременно обновляет собственное состояние.

Но исходный критерий завершения слишком строгий. Torrent не обязан завершаться только при буквальном импорте каждого видеофайла. Безопасными терминальными исходами должны считаться:

- `ImportedAndVerified` — файл импортирован, history и episode-file это подтверждают;
- `AlreadySatisfied` — соответствующий эпизод уже имеет равный или лучший файл, и Sonarr отклонил импорт по ожидаемой причине;
- `IgnoredNonEpisode` — sample, extra, OP/ED или другой явно неэпизодный файл;
- `Blocked` — неоднозначность, неизвестная ошибка, повреждение или опасное несоответствие;
- `Pending` — файл ещё скачивается или операция ещё выполняется.

Финализировать queue item можно, только если нет `Blocked` и `Pending`. Требование «все файлы импортированы» оставит часть season packs висеть навсегда и тем самым не решит вторую заявленную проблему.

### 3. Старый torrent не видит новые файлы rolling release

Формулировка проблемы корректна: повторная проверка старой metadata не может обнаружить файл, которого в этой metadata никогда не было. Для нового файла требуется новая torrent metadata, обычно с новым info hash.

Предложенный общий подход — получить новую metadata, добавить новый torrent в остановленном состоянии и выполнить recheck на существующих данных — разумен как эксперимент. Но он не гарантирует, что будет загружена только разница.

Повторное использование зависит от:

- совпадения физических путей;
- порядка файлов внутри torrent;
- размера piece;
- выравнивания данных по границам pieces;
- конкретных piece hashes;
- версии BitTorrent metadata;
- того, как tracker или release group пересобирает torrent.

Если новая ревизия просто дописывает файл в конец с тем же layout, может переиспользоваться большая часть данных. Если меняются порядок, piece length или layout, recheck может отвергнуть значительную часть даже идентичных файлов.

Поэтому корректная гарантия звучит так:

> Сервис пытается переиспользовать существующие данные и измеряет объём, подтверждённый recheck; в худшем случае новая ревизия может потребовать повторной загрузки.

Утверждения «скачаются только новые или изменённые данные» и «валидные существующие данные обязательно будут переиспользованы» следует удалить из success criteria.

## Проверенные возможности Sonarr

### Явные episode IDs

В актуальном исходном коде Sonarr `ManualImportFile` содержит:

- `Path`;
- `SeriesId`;
- `EpisodeIds`;
- `Quality`;
- `Languages`;
- `ReleaseGroup`;
- `IndexerFlags`;
- `ReleaseType`;
- `DownloadId`.

`ManualImportService` получает указанные episode IDs, строит `LocalEpisode`, повторно запускает import decisions и затем импортирует одобренные файлы.

Отсюда следует важное архитектурное решение: собственный mapper должен определять episode IDs, но не должен самостоятельно имитировать все правила качества, language, custom formats и import rejections Sonarr. Эти решения нужно повторно отдавать на проверку самому Sonarr.

### Рекомендуемый API workflow импорта

Надёжный поток выглядит так:

1. Получить queue item и сохранить `queueId`, `downloadId`, series/season context.
2. Получить список файлов через qBittorrent API.
3. Запросить кандидаты через `GET /api/v3/manualimport`, предпочтительно с `downloadId`, чтобы Sonarr сам применил remote path mapping и нашёл tracked download.
4. Сопоставить относительные пути из qBittorrent manifest с путями, которые видит Sonarr.
5. Построить предлагаемые `episodeIds` и объяснение для dry run.
6. Передать выбранные IDs в `POST /api/v3/manualimport` для reprocess.
7. Не импортировать item, если Sonarr вернул неожиданную rejection.
8. Запустить `ManualImport` через `POST /api/v3/command`.
9. Poll command до терминального результата.
10. Проверить Sonarr history и episode-file state; одного HTTP 2xx недостаточно.
11. Классифицировать каждый файл как imported, already satisfied, ignored, blocked или pending.
12. Когда весь реальный manifest обработан, финализировать queue item через Sonarr queue API.

### Почему нельзя конструировать импорт только из filename parser

Даже при правильном episode ID Sonarr учитывает:

- quality и revision;
- language;
- release group;
- custom format score;
- существующий episode file;
- правила upgrade;
- indexer flags;
- import mode;
- возможные rejections.

Поэтому сервис должен использовать результаты Sonarr manual-import/reprocess как каноническую проверку, а не обходить decision engine.

### Проверка результата

Успех команды ещё не доказывает нужный итог. Проверка должна связывать:

- исходный `downloadId`;
- source path;
- series ID;
- ожидаемые episode IDs;
- появившийся episode-file ID;
- соответствующую запись import history.

Нельзя считать импорт успешным только потому, что команда была принята или завершилась без transport error.

## Проверенные возможности qBittorrent

qBittorrent WebUI API предоставляет необходимые базовые операции:

- получить torrent list и file manifest;
- получить progress и priority отдельных файлов;
- stop/start;
- recheck;
- add torrent;
- set category/tags;
- remove torrent record без удаления данных;
- rename file/folder.

Но наличие API не делает все составные операции атомарными. Например, последовательность из нескольких `renameFile` может завершиться частично, а новый путь может конфликтовать с существующим. Это требует compensation logic и всё равно не даёт настоящей транзакции.

Для qBittorrent 5 терминология API и состояний менялась с paused/resumed на stopped/started. Адаптер должен определять API/version capabilities, а не жёстко ожидать одно название состояния.

## Почему исходные torrent-файлы не нужно переименовывать

Обязательное переименование — главная ненужная сложность исходного концепта.

Если Sonarr получает явные episode IDs, парсабельность исходного имени больше не является условием импорта. Sonarr может создать каноническое имя уже в библиотеке, используя обычную стратегию copy/hardlink. Исходный файл остаётся по пути torrent и продолжает раздаваться.

Преимущества отказа от source rename:

- не изменяется download tree;
- сохраняются оригинальные пути tracker metadata;
- меньше мутаций и меньше сценариев восстановления после сбоя;
- нет batch rename и коллизий;
- легче повторно обработать тот же torrent;
- выше шанс переиспользовать файлы новой ревизией;
- не требуется хранить reversible rename journal в MVP.

Rename может остаться отдельной opt-in функцией только для редких случаев, когда конкретная версия Sonarr не может импортировать файл по явному mapping. Сначала такой случай должен быть воспроизведён интеграционным тестом.

## Источники истины и их границы

Хорошая часть исходного принципа:

- qBittorrent manifest — источник истины о том, какие файлы принадлежат torrent, выбраны ли они и завершены ли они;
- Sonarr — источник истины о series, season, episodes, существующих library files и import policy.

Но этого недостаточно без явных правил:

### Что считать завершённым torrent-файлом

Минимальная проверка должна учитывать не только общий torrent progress, но и файл:

- file priority не равен zero/do-not-download;
- file progress равен 1;
- размер ненулевой и соответствует manifest;
- расширение находится в allowlist поддерживаемых media types;
- файл виден Sonarr через его path namespace;
- Sonarr manual import не сообщает, что файл недоступен.

Общий статус torrent `100%` нельзя использовать как единственное доказательство готовности конкретного файла.

### Что считать действительным медиа

Исходный документ одновременно объявляет media validation обязательным safety invariant и откладывает его до Phase 2. Нужно выбрать одно:

- либо `ffprobe` или аналог входит в MVP;
- либо в MVP доверяем BitTorrent hash verification и Sonarr media analysis, а дополнительный probe называем optional defense-in-depth.

Для первого прототипа разумнее не блокировать разработку собственным медиасканером, а использовать qBittorrent completion, Sonarr decisions и отсутствие import/media errors. Отдельный `ffprobe` добавить после появления реального примера, который эти проверки не ловят.

## Mapping policy

Mapper должен быть консервативным и детерминированным. Confidence score полезен для UI, но автоматическое решение лучше основывать не на произвольном числовом threshold, а на наборе обязательных доказательств.

### Допустимые автоматические mappings

- явный `SxxEyy` или `x`-pattern совпал с series context;
- air date однозначно соответствует одному эпизоду Sonarr;
- абсолютный номер однозначно соответствует одному эпизоду и series type допускает absolute numbering;
- локальный `[03]` объединён с подтверждённым season context и соответствует ровно одному эпизоду;
- multi-episode filename соответствует непрерывному и однозначному списку episode IDs;
- пользователь ранее одобрил узко scoped rule, и его область действия точно совпала.

### Недостаточные доказательства

- «это единственное отсутствующее число»;
- совпадение только по порядку файлов;
- совпадение только по размеру;
- нормализованный title без подтверждённого series ID;
- предположение, что первый файл равен первому эпизоду сезона;
- выбор ближайшего air date без точного соответствия;
- глобальное правило для release group, если нумерация различается между сериалами.

### Контекст series/season

Приоритет источников должен быть примерно таким:

1. Sonarr queue item, связанный по download ID.
2. Sonarr grab history, связанная по download ID.
3. Явная enrolment configuration пользователя.
4. Подтверждённая persisted release identity.
5. Torrent title как supporting evidence, но не единственный идентификатор series.

## Path namespaces

В Docker-инсталляциях qBittorrent и Sonarr часто видят один файл под разными абсолютными путями. Концепт упоминает runtime paths, но не определяет, в каком namespace выполняются операции.

Рекомендуемое правило:

- пути qBittorrent API используются только для идентификации torrent-relative files;
- пути для manual import берутся из ответа Sonarr;
- correlation выполняется по нормализованному относительному пути, размеру и download ID;
- сервис не пытается самостоятельно копировать или перемещать media;
- если Sonarr не видит путь, workflow блокируется с понятной диагностикой remote path mapping.

Это позволяет первому MVP вообще не монтировать download directory внутрь сервиса, если qBittorrent и Sonarr API предоставляют достаточно данных. Файловый mount понадобится только для собственного media probe или другой прямой работы с содержимым.

## Идемпотентность

После отказа от rename задача становится заметно проще.

Возможный логический ключ file workflow:

```text
(sonarrInstanceId, downloadId, torrentInfoHash, normalizedRelativePath, fileSize)
```

Mapping decision отдельно связывается с отсортированным списком episode IDs и версией rule/mapper. До новой import-команды сервис проверяет:

- не появился ли уже нужный episode file;
- нет ли подтверждённой import history;
- не выполняется ли команда с тем же idempotency key;
- не завершался ли item как `AlreadySatisfied`;
- не изменился ли manifest под тем же наблюдаемым workflow.

После API timeout нельзя слепо повторять mutation: сначала проверяется postcondition.

SQLite оправдан после рабочего spike. Для первого одноразового dry-run и одной import-команды можно обойтись без workflow engine. Нельзя проектировать большой state machine до подтверждения API-потока.

## Исправленная архитектура MVP

### Обязательные части

#### Sonarr client

- queue/history/series/episodes/episode-files;
- manual import discovery и reprocess;
- запуск и polling commands;
- проверка history и episode files;
- безопасная queue finalization.

#### qBittorrent client

- read-only torrent list и file manifest;
- version/capability discovery;
- никаких rename/remove/add операций в первом MVP.

#### Mapper

- небольшой набор детерминированных patterns;
- explanation/evidence для каждого решения;
- hard stop при неоднозначности.

#### Orchestrator

- dry run;
- выполнение одного явно выбранного download;
- postcondition checks;
- структурированный результат по каждому файлу.

### Что не нужно в первом MVP

- web UI;
- source adapters;
- автоматическое обнаружение новых torrent revisions;
- замена старого torrent новым;
- file/folder rename;
- tracker credentials;
- универсальный rules engine;
- большой workflow framework;
- удаление torrent records или media data;
- автоматическое периодическое сканирование всех категорий.

## Предлагаемые этапы

### Phase 0: Integration spike

Цель — доказать основной API workflow на одной реальной раздаче.

Функции:

- конфигурация Sonarr и qBittorrent endpoints;
- выбор одного download ID или queue ID;
- чтение manifest;
- определение series/season из Sonarr;
- mapping только одного подтверждённого filename convention;
- подробный dry run;
- opt-in execute;
- explicit ManualImport;
- проверка результата;
- queue finalization через Sonarr;
- отсутствие любых source renames и deletes.

Phase 0 считается успешным, если:

1. `[NN].mkv` корректно сопоставлен эпизоду на основании подтверждённого season context.
2. Sonarr импортировал файл и создал ожидаемый episode-file.
3. Исходный torrent продолжает seeding.
4. Queue item исчез через штатный Sonarr workflow.
5. Повторный запуск не создаёт duplicate import.
6. Неверный или неоднозначный mapping останавливается до mutation.

### Phase 1: Small reliable importer

Добавить только после успешного Phase 0:

- несколько стандартных mapping patterns;
- SQLite state;
- polling управляемой category/tag или explicit allowlist;
- safe terminal outcomes;
- structured audit log/status endpoint;
- restart recovery;
- несколько Sonarr/qBittorrent version contract tests.

### Phase 2: Review and rules

- review API или минимальный UI;
- narrowly scoped approved rules;
- rules versioning и invalidation;
- optional media probe;
- защита от stale mapping после изменения Sonarr metadata.

### Отдельный эксперимент: Rolling revisions

Не следует считать автоматическим продолжением импортера. Сначала нужен прототип для одного конкретного tracker/indexer.

Эксперимент должен ответить:

- есть ли стабильный topic/release identifier;
- как обнаружить и скачать новую metadata;
- меняется ли title/GUID/download URL;
- сохраняются ли paths, order и piece length;
- какой процент данных реально подтверждает recheck;
- что происходит с removed/replaced files;
- можно ли безопасно держать две revisions на одном data tree;
- когда старый torrent можно удалить без удаления данных;
- как новая revision снова попадает в Sonarr tracking и history.

Только после измерения этих свойств стоит проектировать source-adapter interface.

## Rolling torrent: отдельные риски

### Release identity

Нормализованный title недостаточен: возможны разные encodes, qualities, release groups и релизы с похожими именами. GUID также не обязательно стабилен между revisions.

Хорошая identity должна включать adapter-specific stable key. Если tracker его не предоставляет, enrolment может потребовать явную конфигурацию пользователя. Универсальная эвристика без такого ключа опасна.

### Совместное data tree

Два torrent records могут указывать на пересекающиеся файлы. Пока один torrent пишет данные, старый torrent должен быть остановлен. Нельзя возвращать старый torrent в seeding до проверки, что его pieces остались валидны.

Если новая revision заменила существующий файл тем же путём, она может сделать данные старой revision невалидными. Просто «pause old, add new, then remove old» недостаточно; нужны проверки конфликтующих file paths и changed hashes.

### Canonical renames

Применение одинаковых canonical qBittorrent path mappings к новой revision не является бесплатной операцией. Если canonical destination уже существует, rename может конфликтовать. Если metadata новой revision использует оригинальные имена, ранее выполненные renames уменьшают шанс повторного использования.

Это ещё одна причина не переименовывать download tree в import MVP.

### Retirement

Удаление старого torrent record с `deleteFiles=false` безопаснее удаления данных, но всё равно может оставить orphan files. Это допустимо для начальной версии, если явно отражено в audit log. Любая автоматическая очистка должна быть отдельной opt-in policy.

## Тестирование

### Обязательные contract tests

- Sonarr manual-import discovery по `downloadId`;
- manual-import reprocess с явными episode IDs;
- запуск `ManualImport` command;
- polling success/failure;
- history и episode-file verification;
- queue finalization без удаления torrent;
- qBittorrent manifest с selected/unselected и complete/incomplete files;
- API differences поддерживаемых qBittorrent versions.

### Mapping fixtures

- `S02E03`;
- `2x03`;
- `[03]` + season context;
- absolute anime number;
- air date;
- multi-episode range;
- specials;
- duplicate local numbers в разных seasons;
- sample и extra;
- неоднозначный title;
- episode already satisfied;
- file отсутствует в Sonarr path namespace.

### Failure and retry tests

- Sonarr принял command, но client получил timeout;
- процесс перезапущен во время command;
- import завершился частично;
- queue item исчез до finalization;
- qBittorrent manifest изменился между dry run и execute;
- episode metadata изменилась между mapping и import;
- duplicate polling events;
- unexpected rejection;
- Sonarr history появилась позже episode-file или наоборот.

### Rolling experiment tests

- файл добавлен в конец;
- файл вставлен в середину;
- файл заменён под тем же именем;
- файл удалён;
- изменён порядок файлов;
- изменён piece length;
- старая и новая revisions используют одинаковый path;
- новая revision recheck подтверждает 100%, частично или 0% старых данных.

## Безопасность

Сохраняются хорошие инварианты исходного документа:

- никогда не удалять media data по умолчанию;
- не импортировать неоднозначный mapping;
- не считать HTTP acceptance доказательством результата;
- проверять postcondition после mutation;
- не логировать credentials или authorization headers;
- dry run до execute;
- повторные запуски не должны дублировать import;
- tracker-specific secrets не должны попадать в core domain или fixtures.

Дополнения:

- execute должен повторно проверить, что manifest совпадает с dry run;
- разрешённые paths должны находиться под ожидаемым download root;
- нельзя принимать абсолютный путь из torrent metadata как доверенный target для filesystem mutation;
- queue finalization разрешена только для queue/download ID, который был обработан текущим workflow;
- direct qBittorrent delete API не нужен импортному MVP вообще;
- если Sonarr уже потерял tracked download, сервис не должен имитировать его завершение без отдельного explicit recovery режима.

## Наблюдаемость

На каждый workflow достаточно хранить компактную, но точную timeline:

- download и torrent IDs;
- Sonarr series/season context и источник этого контекста;
- hash manifest snapshot;
- каждый рассмотренный media file;
- mapping evidence;
- Sonarr reprocess rejections;
- command ID;
- verification evidence;
- terminal outcome;
- причина queue finalization или отказа от неё.

В MVP status API может просто возвращать эти записи в JSON. Web UI до появления реальных manual-review cases не нужен.

## Аналоги и необходимость отдельного проекта

Предварительный поиск не обнаружил зрелого инструмента, который одновременно:

- выполняет explicit episode mapping для плохо названных Sonarr downloads;
- корректно завершает queue item без удаления torrent;
- отслеживает revisions rolling torrents.

Существуют queue cleaners и download managers вроде Cleanuparr, а также отдельные пользовательские scripts/gists для manual import. Они подтверждают существование класса проблем, но не выглядят полной заменой описанного узкого importer workflow.

Этот вывод не является исчерпывающим market research. Перед публикацией большого open-source продукта стоит проверить сообщества Sonarr, qBittorrent, anime trackers и списки `awesome-arr`, используя несколько конкретных примеров поведения.

## Go / No-Go

### Делать

- Phase 0 для реальной раздачи, которая сейчас требует еженедельного ручного импорта;
- read-only manifest inspection;
- явный episode mapping;
- Sonarr-native manual import;
- Sonarr-native queue finalization;
- dry run и проверку результата;
- conservative stop on ambiguity.

### Пока не делать

- обязательные source renames;
- generic source-adapter framework;
- автоматическую замену rolling torrents;
- web UI;
- универсальную поддержку всех naming schemes;
- собственную library management;
- удаление или cleanup данных;
- обещание «скачивается только разница»;
- большую state-machine архитектуру до первого успешного end-to-end test.

### Условия перехода к полноценному сервису

Продолжать разработку имеет смысл, если Phase 0:

- решает хотя бы три реальные повторяющиеся раздачи;
- не даёт ошибочного импорта;
- переживает повторный запуск;
- сохраняет seeding;
- корректно обрабатывает already-satisfied episodes;
- имеет понятный failure report вместо ручного разбора логов.

Если проблема существует только в воображаемых fixtures и нет настоящих загрузок, которые этот workflow улучшает, отдельный сервис делать не стоит.

## Предлагаемое изменение исходного позиционирования

Вместо обещания решить сразу импорт и mutable torrents проект лучше описывать так:

> `sonarr-torrent-importer` — консервативный companion для автоматизации тех manual-import решений, которые можно однозначно вывести из qBittorrent manifest и Sonarr metadata. Он передаёт явные episode identities в Sonarr, проверяет результат и безопасно завершает queue item, не изменяя и не удаляя torrent data.

Rolling revision management следует описывать как отдельный experimental extension:

> Для явно enrolled sources сервис может экспериментально обнаруживать новые torrent revisions и пытаться переиспользовать данные через qBittorrent recheck. Степень reuse зависит от metadata layout и не гарантируется.

## Итог

Исходный концепт хорошо определяет пользовательскую боль, разделяет зоны ответственности систем и уделяет внимание идемпотентности и безопасности. Его слабость — преждевременное объединение проверяемого importer workflow с существенно менее определённой задачей rolling revisions.

Самый ценный следующий шаг — не выбирать язык и framework, а доказать один end-to-end поток без переименования файлов:

```text
qBittorrent manifest
  -> Sonarr series/season context
  -> deterministic episode IDs
  -> Sonarr reprocess
  -> Sonarr ManualImport command
  -> history/episode-file verification
  -> Sonarr queue finalization
  -> torrent продолжает seeding
```

Если этот поток работает на реальных данных, проект имеет смысл. Если нет, большая архитектура вокруг него лишь закрепит неверные предположения.

## Проверенные источники

- Sonarr API documentation: https://sonarr.tv/docs/api/index
- Sonarr `ManualImportFile` с явными `EpisodeIds`: https://github.com/Sonarr/Sonarr/blob/be1dc0374a0ce6ea23ba88cd7fb57675d5b9ea1e/src/NzbDrone.Core/MediaFiles/EpisodeImport/Manual/ManualImportFile.cs
- Sonarr `ManualImportService`: https://github.com/Sonarr/Sonarr/blob/be1dc0374a0ce6ea23ba88cd7fb57675d5b9ea1e/src/NzbDrone.Core/MediaFiles/EpisodeImport/Manual/ManualImportService.cs
- Sonarr queue finalization: https://github.com/Sonarr/Sonarr/blob/be1dc0374a0ce6ea23ba88cd7fb57675d5b9ea1e/src/Sonarr.Api.V3/Queue/QueueController.cs
- Sonarr qBittorrent `MarkItemAsImported`: https://github.com/Sonarr/Sonarr/blob/be1dc0374a0ce6ea23ba88cd7fb57675d5b9ea1e/src/NzbDrone.Core/Download/Clients/QBittorrent/QBittorrent.cs
- qBittorrent WebUI API 5.0: https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29
- Пример проблемы с частичным season-pack import: https://github.com/Sonarr/Sonarr/issues/5625
- Cleanuparr как смежный queue/download manager: https://github.com/Cleanuparr/Cleanuparr
