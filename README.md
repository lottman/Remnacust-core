[Русский](README.md) · [English](README.en.md)

# Remnacust Core

Форк Xray-core для Remnacust: клиентские соединения, маршрутизация, runtime-применение конфигурации и политики хостов/устройств. Репозиторий также содержит схему и браузерный валидатор редактора панели.

## Стабильный комплект

| Компонент | Опубликованная версия |
| --- | --- |
| Core | [1.1.13](https://github.com/lottman/Remnacust-core/releases/tag/v1.1.13) |
| Основа Xray | [v26.10.31](https://github.com/XTLS/Xray-core/releases/tag/v26.10.31) |
| Node | [1.1.22](https://github.com/lottman/Remnacust-node/releases/tag/v1.1.22) |
| Панель и страница подписки | [1.1.7.97](https://github.com/lottman/Remnacust-panel/releases/tag/v1.1.7.97) |
| Installer | [1.2.136](https://github.com/lottman/Remnacust-installer/releases/tag/v1.2.136) |

Основа — опубликованный официальный Xray v26.10.31, коммит `a4a3dff5b20c2faae567b742c172c7b95e75c083`. Происхождение: [REMNACUST-UPSTREAM.json](xray/REMNACUST-UPSTREAM.json); [изменения Core 1.1.13](docs/RELEASE-1.1.13.md). Новые исходники в ветке не означают обновление опубликованного бинарника.

## Установка и обновление

Для VPS используйте готовый комплект из [инструкции Node](https://github.com/lottman/Remnacust-node). Стабильный Installer 1.2.136 поддерживает Ubuntu 22.04/24.04/26.04 LTS, Linux amd64/arm64. Debian 12/13 добавлен локально, но ещё не опубликован. Начальный ориентир ноды — 1 CPU и 1 GiB RAM без гарантии ёмкости; Node.js и Go на хосте не нужны.

Релизы Core содержат Linux-бинарники amd64/arm64 и `SHA256SUMS`. Для управляемой ноды выбирайте проверенное ядро через панель или `sudo remnacust manage-node`; не подменяйте работающий бинарник вручную. Обновление панели не обновляет удалённые ноды. `sudo remnacust upgrade-node` обновляет образ агента со встроенным Core, но отдельно выбранное managed-ядро может оставаться закреплённым. Проверяйте активные версию и режим.

Сохраняйте `/opt/remnacust/node/.env`, `SECRET_KEY`, Compose и всё приватное состояние: `/var/lib/remnacust/run`, `/var/lib/remnanode`, `/var/lib/remnawave/core-manager`. Installer выносит его в постоянные каталоги рядом с Compose; пути перечислены в `/opt/remnacust/FILES.txt`. Старую ручную установку без mounts сначала остановите и сохраните состояние с владельцами и правами до recreate. Не скрывайте единственную копию пустыми mounts и не удаляйте журналы квот ради отката.

## Возможности и совместимость

- Xera HTTP: `network: "xera-http"` и `xeraHttpSettings`; нужны совместимые ядра клиента и сервера. Стандартный XHTTP — отдельный транспорт.
- Политики хостов/тегов, квоты и пределы скорости пользователя и устройства. Дополнительные соединения не увеличивают выделенный бюджет.
- Отзыв доступа по авторизованному ключу без отключения остальных устройств за тем же NAT.
- Совместимое runtime-применение может сохранять соединения. Смена listener, порта, транспорта или процесса требует планирования перерыва.

Стандартные конфигурации сохраняют legacy-совместимость в пределах возможностей клиента/ядра. Сохранены совместимые варианты XDNS, TUN, QUIC udpHop и сегментное объявление исходящего XDRIVE, перечисленные в provenance. Обычный upstream Xray не получает расширения Remnacust автоматически; не обходите проверки сохранённых политик для установки неподходящего ядра. В Xray v26.10.31 MPTCP выключен по умолчанию; обновление Core не заменяет обновление ядра Linux.

[Совместимость](docs/COMPATIBILITY.md) · [Графы профилей](docs/PROFILE-GRAPHS.md) · [Области лимитов](https://github.com/lottman/Remnacust-panel/blob/main/docs/HOST-LIMITS.md).

## Работа без панели

Действующие соединения и политики могут работать без панели, пока действуют права и локальные гранты. Срок доступа, остаток применимых USER/HOST/TAG-квот и границы квотного периода продолжают ограничивать трафик. Новый календарный период без панели не создаёт новый кредит.

Для суток автономности нужен запас срока и локального кредита на трафик и восстановление. Restart требует совместимых Node/Core, сохранённых конфигурации, credentials и журналов. Нельзя гарантировать 24 часа для любого прежнего ядра или после потери состояния. [Условия автономности](https://github.com/lottman/Remnacust-panel/blob/main/docs/NODE-AUTONOMY.md).

## Ещё не опубликовано

Локальная ветка содержит исправления владения MUX-сессией и её quota lease, ограничение чтения незавершённого Xera packet-up до 300 секунд с освобождением ресурсов при отмене (без общего таймаута для streaming/long polling), исправления распаковки сертификатов uTLS на совместимой основе и обновления Brotli/compress. Новый профиль Xera требует согласованного обновления клиента и сервера. Исправление восстановления квот после истечения части грантов требует новых согласованных Node/Core. **Эти изменения ещё не входят в опубликованный Core 1.1.13**; локальная сборка не обновляет релизы или установленные образы.

## Сборка и проверка

Нужен **Go 1.27.3**. Клонируйте весь репозиторий: `xray/go.mod` использует относительные пути к `vendor/olcrtc` и `vendor/utls`. Имя модуля `github.com/xtls/xray-core` сохранено для совместимости импортов.

```bash
git clone https://github.com/lottman/Remnacust-core.git
cd Remnacust-core/xray
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -o bin/xray ./main
./bin/xray version
./bin/xray run -test -config /path/to/config.json
```

Команда собирает текущий checkout. Номер в `VERSION` сам по себе не доказывает соответствие сборки опубликованному релизу. `-test` проверяет конфигурацию, но не доступность DNS, сертификатов или удалённого сервера. Для управляемой ноды меняйте профиль через панель: агент формирует служебную runtime-конфигурацию.

Из каталога `xray`:

```bash
go test -race ./infra/conf ./core ./app/dispatcher ./app/stats ./app/stats/command \
  ./app/proxyman/inbound ./app/proxyman/outbound ./app/proxyman/command \
  ./transport/internet/splithttp ./transport/internet/xerahttp ./transport/internet/xdrive
```

Сетевые проверки выполняйте в изолированной Linux-среде. [Сборка](docs/BUILDING.md) · [Source checks](.github/workflows/check.yml). Обновление встроенного Core в Node требует отдельной перепаковки проверяемого архива и пересборки образа.

## Редактор панели

Схема и WASM используют те же строители конфигурации, что native Core. Сборка редактора требует Go 1.27.3, Node.js 24, Python 3 и зависимостей frontend панели; это инструменты разработки, а не требования к VPS. Соседний checkout панели определяется автоматически, другой путь задаётся через `REMNACUST_PANEL_SOURCE`.

Из корня репозитория:

```bash
bash editor/go/build-remnacust.sh
```

Скрипт обновляет схему, WASM и проверяет ресурсы панели; затем пересоберите frontend. [README редактора](editor/README.md). Изменение только документации не требует пересборки бинарников или архивов.

## Авторство и лицензии

Remnacust поддерживается lottman. Сохраняются авторство [XTLS/Xray-core](https://github.com/XTLS/Xray-core) и участников проекта, лицензия [MPL-2.0](xray/LICENSE) и исходные notices. Редактор и зависимости сохраняют собственные лицензии. [NOTICE.md](NOTICE.md) · [Панель](https://github.com/lottman/Remnacust-panel) · [Нода](https://github.com/lottman/Remnacust-node) · [Поддержка](https://t.me/lottman).
