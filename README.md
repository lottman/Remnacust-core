# Remnacust Core

Форк Xray-core для Remnacust: клиентские соединения, маршрутизация, runtime-применение и политики хостов/устройств. В репозитории также находятся строители схемы и браузерный валидатор панели.

## Текущий стабильный комплект

**[Core 1.1.12](https://github.com/lottman/Remnacust-core/releases/tag/v1.1.12)** основан на опубликованном стабильном **Xray v26.10.31**, коммит `a4a3dff5b20c2faae567b742c172c7b95e75c083`. Происхождение и совместимые расширения записаны в [REMNACUST-UPSTREAM.json](xray/REMNACUST-UPSTREAM.json).

Согласованный комплект: [Node 1.1.20](https://github.com/lottman/Remnacust-node/releases/tag/v1.1.20), [панель/страница 1.1.7.86](https://github.com/lottman/Remnacust-panel/releases/tag/v1.1.7.86), [установщик 1.2.122](https://github.com/lottman/Remnacust-installer/releases/tag/v1.2.122). Версии компонентов нумеруются отдельно.

В основе используются опубликованные официальные теги Xray, без произвольного переноса последующего `main`. В 26.10.31 MPTCP выключен по умолчанию; обновление приложения не заменяет обновление ядра Linux. Наш исходящий XDRIVE сохраняет прежнее сегментное объявление для совместимости со старыми серверами; новые режимы не включаются автоматически. [Изменения Core 1.1.12](docs/RELEASE-1.1.12.md).

## Установка готового ядра

Релиз Core содержит Linux-бинарники amd64/arm64 и `SHA256SUMS`. Они доступны в проверенном каталоге панели. Установка новой панели сама по себе не обновляет удалённые ноды; используйте управление Core либо `remnacust upgrade-node` для агента вместе со встроенным ядром.

Выбор ядра должен учитывать сохранённые политики, квоты и возможности агента. Не заменяйте управляемый бинарник обходным способом во время работы ноды.

## Сборка

Нужен **Go 1.27.3**. Имя модуля `github.com/xtls/xray-core` сохранено для совместимости импортов. `vendor/olcrtc` входит в checkout; сохраняйте относительный путь из `xray/go.mod`.

```bash
git clone https://github.com/lottman/Remnacust-core.git
cd Remnacust-core/xray
mkdir -p bin
CGO_ENABLED=0 go build -buildvcs=false -trimpath \
  -ldflags '-s -w -X github.com/xtls/xray-core/core.build=Remnacust-1.1.12' \
  -o bin/xray ./main
./bin/xray version
```

В PowerShell задайте `$env:CGO_ENABLED='0'`, передайте те же аргументы `go build` и используйте `bin/xray.exe`. Для установленной ноды отдельная компиляция на VPS не нужна.

## Проверка и запуск

```bash
./bin/xray run -test -config /path/to/config.json
./bin/xray run -config /path/to/config.json
```

`-test` проверяет конфигурацию, но не доступность DNS, сертификатов и удалённых серверов. Для управляемой ноды меняйте профиль в панели: агент формирует служебную runtime-конфигурацию.

## Возможности и границы

- Xera HTTP: `network: "xera-http"` и `xeraHttpSettings`; нужны совместимые ядра обеих сторон.
- Политики хостов/тегов, квоты и отдельные пределы скорости пользователя и устройства. Активные ключи делят пользовательский бюджет; открытие дополнительных потоков не увеличивает долю.
- Отзыв доступа по авторизованному ключу, без отключения остальных устройств за тем же IP.
- Совместимое runtime-применение сохраняет действующие соединения. Смена порта/транспорта listener и замена процесса требуют отдельного обслуживания.
- Генератор панели использует `IPOnDemand` для GeoIP перед общим TCP/UDP-маршрутом; собственные ручные правила сохраняют свой приоритет.

Внутренние протоколы учёта, recovery и независимые глобальные графы имеют отдельные ограничения. Наличие реализации в исходниках не означает автоматическое включение всей схемы в production. [Совместимость](docs/COMPATIBILITY.md), [графы профилей](docs/PROFILE-GRAPHS.md), [области лимитов](https://github.com/lottman/Remnacust-panel/blob/main/docs/HOST-LIMITS.md).

## Редактор панели

WASM и схема собираются теми же строителями конфигурации, что и native Core. Соседний checkout `Remnacust-panel` определяется автоматически; иной путь задаётся через `REMNACUST_PANEL_SOURCE`.

```bash
bash editor/go/build-remnacust.sh
```

Команда обновляет схему, `main.wasm` и `wasm_exec.js`, затем проверяет ресурсы панели. После этого пересоберите frontend. Для изменения ядра ноды отдельно пересоздаётся её проверяемый source-архив. Изменения только README не требуют обновления выпущенных бинарников, тегов или архивов.

[README редактора](editor/README.md) · [Сборка](docs/BUILDING.md).

## Проверки

Из каталога `xray`:

```bash
go test -race ./infra/conf ./core ./app/dispatcher ./app/stats ./app/stats/command \
  ./app/proxyman/inbound ./app/proxyman/outbound ./app/proxyman/command \
  ./transport/internet/splithttp ./transport/internet/xerahttp ./transport/internet/xdrive
```

Полный набор native/WASM, CLI, проверки реального loopback-трафика и зависимостей закреплён в [Source checks](.github/workflows/check.yml). Это не проверка системного VPN-туннеля каждого мобильного приложения.

Публикация бинарников выполняется отдельным ручным workflow по существующему тегу. Коммит документации не создаёт новый релиз и не заменяет опубликованные файлы.

## Лицензии

Сохраняются [MPL-2.0 Xray-core](xray/LICENSE), авторство и лицензии зависимостей. Документация upstream и vendored olcRTC сохраняется отдельно. [NOTICE.md](NOTICE.md) · [Панель](https://github.com/lottman/Remnacust-panel) · [Нода](https://github.com/lottman/Remnacust-node) · [Поддержка](https://t.me/lottman).
