# Исходники Remnacust Core

**Core 1.1.16**, основа — опубликованный стабильный **Xray v26.10.31**, коммит `a4a3dff5b20c2faae567b742c172c7b95e75c083`.

Каталог содержит Go-модуль `github.com/xtls/xray-core` и расширения Remnacust. Точный upstream и совместимость: [REMNACUST-UPSTREAM.json](REMNACUST-UPSTREAM.json). Относительная зависимость `../vendor/olcrtc` должна оставаться доступной при сборке.

Нужен Go 1.27.3. Команды сборки, проверки, установки готовых бинарников и обновления WASM находятся в [README репозитория](../README.md) и [BUILDING.md](../docs/BUILDING.md). Управляемая нода использует конфигурацию, подготовленную агентом; ручной запуск отдельного процесса не заменяет её штатное управление.

Сохраняются [MPL-2.0](LICENSE) и авторство upstream; [NOTICE.md](../NOTICE.md).

## Установка готового комплекта

Скачать последний стабильный установщик и открыть его интерактивное меню:

```bash
curl -fsSLo installer.sh https://github.com/lottman/Remnacust-installer/releases/latest/download/installer.sh && sudo bash installer.sh
```
