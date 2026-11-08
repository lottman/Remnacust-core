[Русский](README.md) · [English](README.en.md)

# Редактор конфигураций Remnacust

Валидатор и схема собираются из текущего **Remnacust Core 1.1.16 / Xray v26.10.31**. После изменения Core пересоберите и поставляйте их вместе; обновление версии исходников не заменяет прежний WASM панели. Production-интерфейс редактора находится в [Remnacust-panel](https://github.com/lottman/Remnacust-panel/tree/main/panel/frontend); этот каталог содержит Go/WASM-сборку и исходный самостоятельный пример Monaco.

## Установка готового комплекта

Скачать последний стабильный установщик и открыть его интерактивное меню:

```bash
curl -fsSLo installer.sh https://github.com/lottman/Remnacust-installer/releases/latest/download/installer.sh && sudo bash installer.sh
```

## Обновление WASM и схемы

Панель выбирает custom или default assets по глобальному режиму. `go/build-remnacust.sh` готовит custom Core; `python3 go/build-default.py` из этого каталога собирает `default.wasm`, `xray.default.schema.json` и provenance из закреплённого pristine upstream v26.10.31. Эти два набора ресурсов не взаимозаменяемы; смена режима редактора требует перезагрузки страницы.

Нужны Go 1.27.3, Python 3, Node.js 26.13 LTS и зависимости frontend панели. Клонируйте панель рядом с Core, затем из корня этого репозитория:

```bash
bash editor/go/build-remnacust.sh
```

Для другого расположения сначала задайте `REMNACUST_PANEL_SOURCE=/path/to/Remnacust-panel`. Сборщик готовит browser-совместимое дерево Core, создаёт типы и схему, собирает `main.wasm`, копирует соответствующий `wasm_exec.js` и запускает `verify-editor-assets.mjs` в панели.

Используйте runtime WASM от той же версии Go. Не заменяйте только `.wasm`, оставляя старые схему или `wasm_exec.js`. После обновления ресурсов нужен новый образ панели; обновление Core на ноде выполняется отдельно.

Браузер использует конфигурационные строители ядра, но не открывает native sockets. Успех валидатора не подтверждает доступность домена, действительность сертификата или запуск туннеля Happ/INCY.

[Core README](../README.md) · [Подробности сборки](../docs/BUILDING.md) · [Скрипт](go/build-remnacust.sh) · [Лицензии](../NOTICE.md).
