[Русский](README.md) · [English](README.en.md)

# Редактор конфигураций Remnacust

Валидатор и схема панели соответствуют **Remnacust Core 1.1.13 / Xray v26.10.31**. Production-интерфейс редактора находится в [Remnacust-panel](https://github.com/lottman/Remnacust-panel/tree/main/panel/frontend); этот каталог содержит Go/WASM-сборку и исходный самостоятельный пример Monaco.

## Обновление WASM и схемы

Нужны Go 1.27.3, Python 3, Node.js 24 и зависимости frontend панели. Клонируйте панель рядом с Core, затем из корня этого репозитория:

```bash
bash editor/go/build-remnacust.sh
```

Для другого расположения сначала задайте `REMNACUST_PANEL_SOURCE=/path/to/Remnacust-panel`. Сборщик готовит browser-совместимое дерево Core, создаёт типы и схему, собирает `main.wasm`, копирует соответствующий `wasm_exec.js` и запускает `verify-editor-assets.mjs` в панели.

Используйте runtime WASM от той же версии Go. Не заменяйте только `.wasm`, оставляя старые схему или `wasm_exec.js`. После обновления ресурсов нужен новый образ панели; обновление Core на ноде выполняется отдельно.

Браузер использует конфигурационные строители ядра, но не открывает native sockets. Успех валидатора не подтверждает доступность домена, действительность сертификата или запуск туннеля Happ/INCY.

[Core README](../README.md) · [Подробности сборки](../docs/BUILDING.md) · [Скрипт](go/build-remnacust.sh) · [Лицензии](../NOTICE.md).
