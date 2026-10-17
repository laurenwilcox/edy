#!/bin/bash
# Собирает Edy.app — окно Edy на Wails — для этого Mac.
# Нужны: Go 1.26+ (brew install go) и Xcode Command Line Tools (xcode-select --install).
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v go >/dev/null; then
  echo "Не найден Go. Установите: brew install go" >&2
  exit 1
fi
if ! xcode-select -p >/dev/null 2>&1; then
  echo "Не найдены Xcode Command Line Tools. Установите: xcode-select --install" >&2
  exit 1
fi

VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
APP="build/Edy.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin"

echo "Собираю окно Edy ($VERSION)…"
CGO_ENABLED=1 \
CGO_LDFLAGS="-framework UniformTypeIdentifiers" \
MACOSX_DEPLOYMENT_TARGET=13.0 \
  go build -tags desktop,production -trimpath -ldflags "-s -w" -o "$APP/Contents/MacOS/Edy" ./cmd/edy-app

# Команда edy лежит отдельно от окна: файловая система Mac не различает регистр,
# и «edy» рядом с «Edy» перезаписал бы само приложение.
echo "Собираю команду edy…"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$APP/Contents/Resources/bin/edy" ./cmd/edy

# Проверка: в MacOS/Edy должно лежать окно, а не копия команды edy.
if cmp -s "$APP/Contents/MacOS/Edy" "$APP/Contents/Resources/bin/edy"; then
  echo "Ошибка сборки: вместо окна в бандле оказалась команда edy" >&2
  exit 1
fi

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Edy</string>
  <key>CFBundleDisplayName</key><string>Edy</string>
  <key>CFBundleIdentifier</key><string>dev.edy.ide</string>
  <key>CFBundleExecutable</key><string>Edy</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${VERSION}</string>
  <key>CFBundleVersion</key><string>${VERSION}</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSAppTransportSecurity</key>
  <dict>
    <!-- Окно открывает интерфейс Edy с http://127.0.0.1 -->
    <key>NSAllowsLocalNetworking</key><true/>
  </dict>
</dict>
</plist>
PLIST

# Подпись «для себя», чтобы macOS запускала приложение без лишних вопросов.
codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || true

echo
echo "Готово: $APP"
echo "Запуск:      open $APP"
echo "Установить:  rm -rf /Applications/Edy.app && cp -R $APP /Applications/"
echo "Команда edy: $APP/Contents/Resources/bin/edy"
