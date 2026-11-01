#!/usr/bin/env bash
# Prepara un VPS Linux para despeje: crea un usuario que solo puede abrir
# túneles SSH (port forwarding), sin shell ni nada más. Idempotente.
#
#   curl -fsSL https://raw.githubusercontent.com/Joseluk/despeje/main/setup-vps.sh | sudo bash -s -- 'ssh-ed25519 AAAA... despeje@mipc'
#
# Quitarlo:
#   curl -fsSL https://raw.githubusercontent.com/Joseluk/despeje/main/setup-vps.sh | sudo bash -s -- --remove
#
# Variables: DESPEJE_USER (por defecto "despeje").
set -euo pipefail

USER_NAME="${DESPEJE_USER:-despeje}"
DROPIN="/etc/ssh/sshd_config.d/10-$USER_NAME.conf"
MAIN_CFG=/etc/ssh/sshd_config
MARK="# despeje-user:$USER_NAME"

say()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx\033[0m  %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "ejecútalo como root (con sudo)"
[[ "$USER_NAME" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "DESPEJE_USER inválido: $USER_NAME"

SSHD="$(command -v sshd || true)"
for c in /usr/sbin/sshd /usr/local/sbin/sshd; do [ -n "$SSHD" ] || { [ -x "$c" ] && SSHD="$c"; }; done
[ -n "$SSHD" ] || die "no encuentro sshd: ¿hay un servidor OpenSSH instalado?"

reload_sshd() {
	"$SSHD" -t || return 1
	if command -v systemctl >/dev/null 2>&1; then
		systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || return 1
	elif command -v rc-service >/dev/null 2>&1; then
		rc-service sshd reload
	elif command -v service >/dev/null 2>&1; then
		service ssh reload 2>/dev/null || service sshd reload
	else
		kill -HUP "$(cat /run/sshd.pid 2>/dev/null || cat /var/run/sshd.pid)"
	fi
}

effective() { "$SSHD" -T -C "user=$USER_NAME,host=localhost,addr=127.0.0.1" 2>/dev/null; }

# Deshace lo que añadimos a la config de sshd (drop-in o línea marcada).
remove_allowusers() {
	rm -f "$DROPIN"
	if grep -q "$MARK" "$MAIN_CFG" 2>/dev/null; then
		sed -i "/$MARK\$/d" "$MAIN_CFG"
	fi
}

if [ "${1:-}" = "--remove" ]; then
	remove_allowusers
	reload_sshd || warn "no se pudo recargar sshd; revisa con: $SSHD -t"
	if id "$USER_NAME" >/dev/null 2>&1; then
		pkill -u "$USER_NAME" 2>/dev/null && sleep 1 || true # túneles abiertos
		userdel -r "$USER_NAME" 2>/dev/null || deluser --remove-home "$USER_NAME"
	fi
	say "despeje eliminado del VPS"
	exit 0
fi

PUBKEY="${1:-}"
[ -n "$PUBKEY" ] || die "falta la clave pública: ... | sudo bash -s -- 'ssh-ed25519 AAAA...'"
[[ "$PUBKEY" =~ ^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh.com)\ [A-Za-z0-9+/=]+(\ [A-Za-z0-9._@-]+)?$ ]] \
	|| die "eso no parece una clave pública SSH de una sola línea"

# 1. Usuario sin shell
NOLOGIN="$(command -v nologin || true)"
for c in /usr/sbin/nologin /sbin/nologin; do [ -n "$NOLOGIN" ] || { [ -x "$c" ] && NOLOGIN="$c"; }; done
[ -n "$NOLOGIN" ] || NOLOGIN=/bin/false
if id "$USER_NAME" >/dev/null 2>&1; then
	say "el usuario $USER_NAME ya existe"
else
	if command -v useradd >/dev/null 2>&1; then
		useradd -m -s "$NOLOGIN" "$USER_NAME"
	else
		adduser -D -s "$NOLOGIN" "$USER_NAME" # Alpine/BusyBox
	fi
	# Algunas distros dejan la cuenta "bloqueada" (!) y sshd la rechaza incluso con clave.
	passwd -d "$USER_NAME" >/dev/null 2>&1 || true
	usermod -p '*' "$USER_NAME" 2>/dev/null || true
	say "usuario $USER_NAME creado (sin shell)"
fi

# 2. Clave: solo port forwarding (sin pty, agente, X11, ni comandos)
HOME_DIR="$(getent passwd "$USER_NAME" | cut -d: -f6)"
install -d -m 700 -o "$USER_NAME" -g "$(id -gn "$USER_NAME")" "$HOME_DIR/.ssh"
AUTH="$HOME_DIR/.ssh/authorized_keys"
LINE="restrict,port-forwarding $PUBKEY"
KEYBODY="$(echo "$PUBKEY" | cut -d' ' -f1-2)"
touch "$AUTH"
if grep -qF "$KEYBODY" "$AUTH"; then
	say "la clave ya estaba autorizada"
else
	echo "$LINE" >>"$AUTH"
	say "clave autorizada (solo túneles)"
fi
chown "$USER_NAME:$(id -gn "$USER_NAME")" "$AUTH"
chmod 600 "$AUTH"

# 3. sshd: si hay AllowUsers, añadimos el usuario (AllowUsers se acumula).
CFG="$(effective)" || die "sshd -T falló; revisa tu configuración con: $SSHD -t"
if grep -qi '^allowusers ' <<<"$CFG" && ! grep -qiE "^allowusers ([^ ]+ )*$USER_NAME( |\$)" <<<"$CFG"; then
	if [ -d /etc/ssh/sshd_config.d ] && grep -qE '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/' "$MAIN_CFG"; then
		printf '%s: usuario solo para túneles\nAllowUsers %s\n' "$MARK" "$USER_NAME" >"$DROPIN"
	else
		# Al principio del fichero para no caer dentro de un bloque Match.
		sed -i "1i AllowUsers $USER_NAME $MARK" "$MAIN_CFG"
	fi
	if reload_sshd; then
		say "añadido $USER_NAME a AllowUsers"
	else
		remove_allowusers
		die "la config de sshd no validó; la he dejado como estaba"
	fi
	CFG="$(effective)"
fi

# 4. Comprobaciones
grep -qi '^allowgroups ' <<<"$CFG" && warn "tienes AllowGroups: añade $USER_NAME a uno de esos grupos"
grep -qi '^denyusers .*\b'"$USER_NAME"'\b' <<<"$CFG" && warn "$USER_NAME aparece en DenyUsers"
FWD="$(awk 'tolower($1)=="allowtcpforwarding"{print tolower($2)}' <<<"$CFG")"
case "$FWD" in
	yes|all|local) ;;
	*) warn "AllowTcpForwarding=$FWD: despeje necesita 'yes' o 'local' para $USER_NAME" ;;
esac
grep -qi '^disableforwarding yes' <<<"$CFG" && warn "DisableForwarding yes impide los túneles"
PO="$(awk 'tolower($1)=="permitopen"{$1="";print}' <<<"$CFG" | xargs)"
[ -z "$PO" ] || [ "$PO" = "any" ] || warn "PermitOpen=$PO limita a dónde puede salir despeje"

PORT="$(awk 'tolower($1)=="port"{print $2; exit}' <<<"$CFG")"
say "listo. En tu Windows usa: usuario '$USER_NAME', puerto ${PORT:-22}"
