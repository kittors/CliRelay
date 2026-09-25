# PostgreSQL managed by Patroni for a multi-instance CliRelay cluster.
#
# The base must stay on the same Alpine (musl) PostgreSQL image family as the
# single-node runtime stack. Physical replication copies index pages byte for
# byte, and text indexes are ordered by the C library's collation: a glibc
# replica of a musl primary (or the reverse) silently returns wrong results
# for indexed text lookups after a failover. Pin the exact minor version too,
# because a standby must not run an older minor than its primary.
ARG POSTGRES_IMAGE=postgres:15.19-alpine3.24
FROM ${POSTGRES_IMAGE}

ARG PATRONI_VERSION=4.1.5

# psycopg ships musllinux wheels, so nothing is compiled here and the image
# does not need a toolchain.
RUN apk add --no-cache python3 curl \
    && python3 -m venv /opt/patroni \
    && /opt/patroni/bin/pip install --no-cache-dir \
        "patroni[etcd3,psycopg3]==${PATRONI_VERSION}" \
    && /opt/patroni/bin/patroni --version \
    && mkdir -p /etc/patroni /var/run/postgresql \
    && chown postgres:postgres /etc/patroni /var/run/postgresql

ENV PATH=/opt/patroni/bin:$PATH

USER postgres
ENTRYPOINT ["/opt/patroni/bin/patroni"]
CMD ["/etc/patroni/patroni.yml"]
