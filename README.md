# Remnacust Core

Форк Xray-core для Remnacust. Обслуживает клиентские соединения, выполняет маршрутизацию и применяет политики хостов, тегов и устройств. Здесь также находятся исходники редактора и браузерной проверки конфигураций.

**Версия 1.1.4** · **Основа: Xray-core 26.9.30** · [Панель](https://github.com/lottman/Remnacust-panel) · [Нода](https://github.com/lottman/Remnacust-node)

Имя Go-модуля `github.com/xtls/xray-core` сохранено для совместимости импортов. Исходники зависимости olcRTC находятся в `vendor/olcrtc`; отдельная загрузка из файлового менеджера не нужна.

## Сборка

Нужен Go 1.27.

```bash
git clone https://github.com/lottman/Remnacust-core.git
cd Remnacust-core/xray
mkdir -p bin
CGO_ENABLED=0 go build -trimpath \
  -ldflags '-s -w -X github.com/xtls/xray-core/core.build=Remnacust-1.1.4' \
  -o bin/xray ./main
./bin/xray version
```

В Windows задайте `$env:CGO_ENABLED='0'` и укажите `bin/xray.exe`. При установке ноды [установщик](https://github.com/lottman/Remnacust-installer) скачивает готовый Docker-образ с агентом и ядром; сборки на сервере нет.

## Проверка и запуск

```bash
./bin/xray run -test -config /path/to/config.json
./bin/xray run -config /path/to/config.json
```

Первая команда проверяет конфигурацию и завершает работу. Она не проверяет внешнюю доступность портов, DNS и сертификатов. Если ядром управляет агент, меняйте рабочий профиль в панели: агент формирует runtime-конфигурацию.

Для расширенных квот, скорости и устройств нужны совместимые версии панели, агента и ядра. Замена только бинарного файла не обновляет агент.

## Редактор панели

Клонируйте `Remnacust-panel` рядом с этим репозиторием. Из корня `Remnacust-core`:

```bash
bash editor/go/build-remnacust.sh
```

Команда обновит схему, `main.wasm` и `wasm_exec.js` в соседней панели. Для другого расположения задайте `REMNACUST_PANEL_SOURCE=/path/to/Remnacust-panel`. Затем пересоберите образ панели. [Исходники редактора](editor/README.md).

## Проверки

Из каталога `xray`:

```bash
go test ./infra/conf ./transport/internet/xerahttp ./app/dispatcher ./app/proxyman/command ./app/commander
```

Версия upstream и дополнительные изменения записаны в [REMNACUST-UPSTREAM.json](xray/REMNACUST-UPSTREAM.json). [Поддержка](https://t.me/lottman).

В 1.1.2 включены исправления официальной ветки Xray по состоянию на коммит `7da5dae6502b787fc6d903863e9a6c5043d107a2`: gRPC, Mux, QUIC, WireGuard, TUN и XDNS. Для XDNS принимаются как новые поля `names`/`addrs`, так и прежние `name`/`type`/`settings.addr`.

## Xera HTTP

Xera HTTP — форк транспорта XHTTP (SplitHTTP) из Xray-core, с собственными настройками `network: "xera-http"` и `xeraHttpSettings`. Для соединения его должны поддерживать ядра обеих сторон. Настройка, отличия, режимы и переход с XHTTP описаны в [руководстве панели](https://github.com/lottman/Remnacust-panel/blob/main/panel/frontend/public/documentation/guide-ru.md#транспорт-xera-http).

## Лицензии

Ядро сохраняет [MPL-2.0](xray/LICENSE) и авторство Xray-core. Редактор и зависимости сохраняют собственные лицензии. Подробности: [NOTICE.md](NOTICE.md).

В 1.1.4 отзыв устройства закрывает и уже открытый поток, включая передачу, обходящую внутренние буферы. Соединения выбираются по авторизованному идентификатору, а не по IP: другие устройства за тем же NAT остаются подключёнными. Повторное разрешение устройства снимает запрет новых подключений. Этот сценарий проверяется настоящими VLESS-соединениями в CI.
