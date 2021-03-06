#!/usr/bin/env python
"""Base class for protocols flavor.

Each set of protocols has a flavor name. Derived classes should call
register_flavor when they are imported.
"""

import logging


class ProtocolsFlavor(object):
  """Base class for protocols flavor."""

  def binlog_player_protocol(self):
    """Tdthe binlog player protocol between vttablets, in go."""
    raise NotImplementedError('Not implemented in the base class')

  def binlog_player_python_protocol(self):
    """The binlog player protocol in for python connections.

    This is for python connections to update_stream service.
    """
    raise NotImplementedError('Not implemented in the base class')

  def vtctl_client_protocol(self):
    """The protocol to use for vtctl connections.

    This is just for the go client.
    """
    raise NotImplementedError('Not implemented in the base class')

  def vtctl_python_client_protocol(self):
    """The protocol to use for vtctl connections.

    This is just for the python client.
    """
    raise NotImplementedError('Not implemented in the base class')

  def vtworker_client_protocol(self):
    """The protocol to use for vtworker connections."""
    raise NotImplementedError('Not implemented in the base class')

  def tablet_manager_protocol(self):
    """The protocol to use for the tablet manager protocol."""
    raise NotImplementedError('Not implemented in the base class')

  def tabletconn_protocol(self):
    """The protocol to use for connections from vtctl/vtgate to vttablet."""
    raise NotImplementedError('Not implemented in the base class')

  def vtgate_protocol(self):
    """The protocol to use to talk to vtgate, in go."""
    raise NotImplementedError('Not implemented in the base class')

  def vtgate_python_protocol(self):
    """The protocol to use to talk to vtgate with python clients."""
    raise NotImplementedError('Not implemented in the base class')

  def rpc_timeout_message(self):
    """The error message used by the protocol to indicate a timeout."""
    raise NotImplementedError('Not implemented in the base class')

  def service_map(self):
    """A list of entries to enable all relevant protocols in all servers."""
    raise NotImplementedError('Not implemented in the base class')


__knows_protocols_flavor_map = {}
__protocols_flavor = None


def protocols_flavor():
  return __protocols_flavor


def set_protocols_flavor(flavor):
  """Set the protocols flavor by flavor name."""
  global __protocols_flavor

  if not flavor:
    flavor = 'gorpc'

  cls = __knows_protocols_flavor_map.get(flavor, None)
  if not cls:
    logging.error('Unknown protocols flavor %s', flavor)
    exit(1)
  __protocols_flavor = cls()

  logging.debug('Using protocols flavor %s', flavor)


def register_flavor(key, cls):
  __knows_protocols_flavor_map[key] = cls
