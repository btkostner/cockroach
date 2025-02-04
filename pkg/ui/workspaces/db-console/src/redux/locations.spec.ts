// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import * as protos from "src/js/protos";
import { ILocation, selectLocations, selectLocationTree } from "./locations";

const Location = protos.cockroach.server.serverpb.LocationsResponse.Location;

/*
 * Some context here will help.  The types generated by Protobuf.js leave a bit
 * to be desired.  We can't use the classes themselves, because any child
 * properties are typed as the interface variant.  And the interface variant
 * isn't really right, because every property is optional, even though, in fact
 * every property is required (at least in our usage).  This means that we can't
 * use the objects easily without excessive null checks.
 *
 * Internally, we've chosen to just hold on to the original protobuf classes
 * everywhere.  It would probably be better software engineering to instead add
 * an Anti-Corruption Layer between our use of protobuf and the core of the
 * front-end code, but that's not what we're doing.
 *
 * So, when we ask for the Locations from the store, we chose to type them as
 * ILocation (which we hide behind the imported type Location, which is,
 * admittedly, probably a bad idea, too).  Unfortunately, since they're actually
 * protobuf-generated classes, they have a bunch of other stuff in addition to
 * the properties we care about.
 *
 * This method turns an ILocation into a plain object, so we can do a regular
 * deepEquals assertion on it, to make the tests somewhat readable.
 * Unfortunately, because TypeScript thinks we're holding on to an ILocation
 * rather than a Location, we can't call toObject on it directly.  Fortunately,
 * there's a fast path in the fromObject code that returns the original object
 * immediately if it is, in fact, an instance of the Location class.
 */
function climbOutOfTheMorass(loc: ILocation): { [k: string]: any } {
  return Location.toObject(Location.fromObject(loc));
}

function makeStateWithLocations(locationData: ILocation[]) {
  return {
    cachedData: {
      locations: {
        data: protos.cockroach.server.serverpb.LocationsResponse.fromObject({
          locations: locationData,
        }),
        inFlight: false,
        valid: true,
        unauthorized: false,
      },
    },
  };
}

describe("selectLocations", function () {
  it("returns an empty array if location data is missing", function () {
    const state = {
      cachedData: {
        locations: {
          inFlight: false,
          valid: false,
          unauthorized: false,
        },
      },
    };

    expect(selectLocations(state)).toEqual([]);
  });

  // Data must still be returned while the state is invalid to avoid
  // flickering while the data is being refreshed.
  it("returns location data if it exists but is invalid", function () {
    const locationData = [
      {
        locality_key: "city",
        locality_value: "nyc",
        latitude: 123,
        longitude: 456,
      },
    ];
    const state = makeStateWithLocations(locationData);
    state.cachedData.locations.valid = false;

    expect(selectLocations(state).map(climbOutOfTheMorass)).toEqual(
      locationData,
    );
  });

  it("returns an empty array if location data is null", function () {
    const state = makeStateWithLocations(null);

    expect(selectLocations(state).map(climbOutOfTheMorass)).toEqual([]);
  });

  it("returns location data if valid", function () {
    const locationData = [
      {
        locality_key: "city",
        locality_value: "nyc",
        latitude: 123,
        longitude: 456,
      },
    ];
    const state = makeStateWithLocations(locationData);

    const result = selectLocations(state).map(climbOutOfTheMorass);

    expect(result).toEqual(locationData);
  });
});

describe("selectLocationTree", function () {
  it("returns an empty object if locations are empty", function () {
    const state = makeStateWithLocations([]);

    expect(selectLocationTree(state)).toEqual({});
  });

  it("makes a key for each locality tier in locations", function () {
    const tiers = ["region", "city", "data-center", "rack"];
    const locations = tiers.map(tier => ({ locality_key: tier }));
    const state = makeStateWithLocations(locations);

    tiers.forEach(tier =>
      expect(Object.keys(selectLocationTree(state))).toContain(tier),
    );
  });

  it("makes a key for each locality value in each tier", function () {
    const cities = ["nyc", "sf", "walla-walla"];
    const dataCenters = ["us-east-1", "us-west-1"];
    const cityLocations = cities.map(city => ({
      locality_key: "city",
      locality_value: city,
    }));
    const dcLocations = dataCenters.map(dc => ({
      locality_key: "data-center",
      locality_value: dc,
    }));
    const state = makeStateWithLocations(cityLocations.concat(dcLocations));

    const tree = selectLocationTree(state);

    expect(Object.keys(tree)).toContain("city");
    expect(Object.keys(tree)).toContain("data-center");
    cities.forEach(city => expect(Object.keys(tree.city)).toContain(city));
    dataCenters.forEach(dc =>
      expect(Object.keys(tree["data-center"])).toContain(dc),
    );
  });

  it("returns each location under its key and value", function () {
    const us = {
      locality_key: "country",
      locality_value: "US",
      latitude: 1,
      longitude: 2,
    };
    const nyc = {
      locality_key: "city",
      locality_value: "NYC",
      latitude: 3,
      longitude: 4,
    };
    const sf = {
      locality_key: "city",
      locality_value: "SF",
      latitude: 5,
      longitude: 6,
    };
    const locations = [us, nyc, sf];
    const state = makeStateWithLocations(locations);

    const tree = selectLocationTree(state);

    expect(climbOutOfTheMorass(tree.country.US)).toEqual(us);
    expect(climbOutOfTheMorass(tree.city.NYC)).toEqual(nyc);
    expect(climbOutOfTheMorass(tree.city.SF)).toEqual(sf);
  });
});
