/usr/local/bin/tailscaled --tun=userspace-networking &
until /usr/local/bin/tailscale up --authkey=${AUTH_KEY} --hostname=${NAME} --advertise-routes=${ROUTES} --accept-dns=false ${EXTRA_ARGS}
do
    sleep 0.1
done

while pgrep tailscaled >/dev/null
do
    sleep 1
done
