#!/bin/sh
# Converts networks_for_libreoffice.csv to libreoffice_from_csv.xlsx using a
# real, headless LibreOffice binary in a container -- nothing installed on
# the host. Run from this directory (internal/onboard/testdata/real/gen).
#
# Image used to generate the committed fixture: linuxserver/libreoffice:latest
# (a GUI-oriented image, ~2.5 GB; overriding its entrypoint runs soffice
# directly without the desktop session). A smaller headless-only image would
# be preferable for routine regeneration but was not evaluated here -- see
# README.md for why this one was used.
set -eu
cd "$(dirname "$0")"
docker run --rm --entrypoint sh -v "$PWD":/work -w /work linuxserver/libreoffice:latest -c \
  'soffice --headless --convert-to xlsx networks_for_libreoffice.csv'
mv networks_for_libreoffice.xlsx ../libreoffice_from_csv.xlsx
echo "wrote ../libreoffice_from_csv.xlsx"
