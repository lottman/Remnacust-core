# Сборка ядра и редактора

Нужен Go 1.27. Из каталога `xray`:

```bash
go test ./infra/conf ./transport/internet/xerahttp ./app/dispatcher ./app/proxyman/command ./app/commander
CGO_ENABLED=0 go build -trimpath -o xray ./main
./xray version
```

Для редактора клонируйте [Remnacust-panel](https://github.com/lottman/Remnacust-panel) рядом и установите зависимости её frontend. Из корня ядра запустите `bash editor/go/build-remnacust.sh`. Для другого расположения панели задайте `REMNACUST_PANEL_SOURCE`. Скрипт обновит WASM, Go runtime для браузера и схему конфигурации.

После изменений ядра обновите исходный архив в соседнем [Remnacust-node](https://github.com/lottman/Remnacust-node): `python3 node/docker/package-remnacust-core.py` из корня ноды. Пересоберите ноду и панель, если изменили редактор.

[Выпуск компонентов](https://github.com/lottman/Remnacust-installer/blob/main/docs/PUBLISHING.md).
