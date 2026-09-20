FROM prom/prometheus:v2.55.0
COPY railway/prometheus.yml /etc/prometheus/prometheus.yml
# Listen on all interfaces (including IPv6) so other Railway services can reach it.
CMD ["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus", "--storage.tsdb.retention.time=3d", "--web.listen-address=:9090"]
